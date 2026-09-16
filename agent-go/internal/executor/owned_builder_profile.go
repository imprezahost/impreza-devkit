package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func ownedBuilderProfileText(work string) string {
	return "abi <abi/4.0>,\nprofile impreza-builder-" + work + " flags=(unconfined) { userns, }\n"
}

func ownedBuilderProfilePresent(work string) (bool, error) {
	f, err := os.Open("/sys/kernel/security/apparmor/profiles")
	if err != nil {
		return false, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 8*1024*1024+1))
	if err != nil {
		return false, err
	}
	if len(raw) > 8*1024*1024 {
		return false, errors.New("AppArmor inventory exceeds limit")
	}
	name := "impreza-builder-" + work
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, name+" ") || line == name || strings.HasPrefix(line, name+"//") {
			return true, nil
		}
	}
	return false, nil
}

// Called under the builder journal lock. -a refuses replacing an existing
// profile. No persistent /etc profile, host sysctl or global builder is changed.
// An ambiguous parser result retains the intent and file for explicit recovery.
func manageOwnedBuilderProfile(ctx context.Context, dir string, r *ownedBuilderRecord, create bool) error {
	if err := r.validate(&r.Work, r.DeploymentID, r.BootID, r.DaemonID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	path := filepath.Join(dir, "builder.apparmor")
	present, err := ownedBuilderProfilePresent(r.Work.ID)
	if err != nil {
		return err
	}
	if create {
		if r.Profile != "planned" || r.Phase != "intent" || present {
			return errors.New("builder profile already exists or creation uncertain")
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = f.WriteString(ownedBuilderProfileText(r.Work.ID))
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = syncRecoveryDirectory(dir); err != nil {
			return err
		}
		if err = exec.CommandContext(ctx, "/usr/sbin/apparmor_parser", "-a", path).Run(); err != nil {
			return errors.New("builder profile load uncertain")
		}
		present, err = ownedBuilderProfilePresent(r.Work.ID)
		if err != nil || !present {
			return errors.New("builder profile load not verified")
		}
		r.Profile = "loaded"
		return writeWorkJSON(dir, "builder.json", r)
	}
	if r.Profile == "" {
		return nil
	} // No profile was created for this record.
	if (r.Phase != "finished" && r.Phase != "stopped" && r.Phase != "recovered") || (r.Profile != "loaded" && r.Profile != "released") {
		return errors.New("builder profile cannot be released before verified stop")
	}
	if r.Profile == "released" {
		if present {
			return errors.New("released builder profile reappeared")
		}
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != int64(len(ownedBuilderProfileText(r.Work.ID))) {
		return errors.New("builder profile source changed")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != ownedBuilderProfileText(r.Work.ID) {
		return errors.New("builder profile source mismatch")
	}
	if present {
		if err = exec.CommandContext(ctx, "/usr/sbin/apparmor_parser", "-R", path).Run(); err != nil {
			return errors.New("builder profile release uncertain")
		}
	}
	present, err = ownedBuilderProfilePresent(r.Work.ID)
	if err != nil || present {
		return errors.New("builder profile release not verified")
	}
	r.Profile = "released"
	return writeWorkJSON(dir, "builder.json", r)
}
