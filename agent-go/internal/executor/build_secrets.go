package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const buildSecretsProtocol = "build-secrets-v1"

var buildSecretName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

func validateBuildSecretNames(names []string) error {
	if len(names) == 0 || len(names) > 16 {
		return errors.New("invalid build secret count")
	}
	seen := map[string]bool{}
	for _, name := range names {
		if !buildSecretName.MatchString(name) || seen[name] {
			return errors.New("invalid build secret name")
		}
		seen[name] = true
	}
	return nil
}

func privateSecretDirectory(app string) (string, error) {
	dir := filepath.Join(app, ".build-secrets")
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("invalid build secret directory")
	}
	return dir, nil
}

// Only private regular files with validated names are removed; never follow links.
func clearBuildSecrets(app string) error {
	dir, err := privateSecretDirectory(app)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range files {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !buildSecretName.MatchString(entry.Name()) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 65536 {
			return errors.New("unexpected file in build secret directory")
		}
	}
	for _, entry := range files {
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(dir)
}

func writeBuildSecrets(app string, names []string, values map[string]string) error {
	if err := validateBuildSecretNames(names); err != nil {
		return err
	}
	actual := make([]string, 0, len(values))
	total := 0
	for name, value := range values {
		if !buildSecretName.MatchString(name) || value == "" || len(value) > 65536 || strings.ContainsRune(value, 0) || !utf8.ValidString(value) {
			return errors.New("invalid build secret response")
		}
		total += len(value)
		actual = append(actual, name)
	}
	want := append([]string(nil), names...)
	sort.Strings(want)
	sort.Strings(actual)
	if total > 262144 || !slices.Equal(want, actual) {
		return errors.New("build secret response does not match reviewed names")
	}
	if err := clearBuildSecrets(app); err != nil {
		return err
	}
	dir := filepath.Join(app, ".build-secrets")
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = clearBuildSecrets(app)
		}
	}()
	for _, name := range want {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = io.WriteString(f, values[name])
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	complete = true
	return nil
}

func (d *Docker) fetchBuildSecrets(ctx context.Context, cmd *sdkclient.PollCommand, p sdkclient.DeployPayload, app string) error {
	build := p.Manifest.Runtime.Build
	if build == nil || len(build.SecretNames) == 0 {
		return nil
	}
	if build.SecretProtocol != buildSecretsProtocol || d.Client == nil || cmd.ControlToken == "" {
		return errors.New("build secrets require a supported, controlled deployment")
	}
	if err := validateBuildSecretNames(build.SecretNames); err != nil {
		return err
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	secrets, err := d.Client.AgentBuildSecrets(fetchCtx, p.DeploymentID, cmd.ID, cmd.ControlToken)
	if err != nil {
		return errors.New("build credentials could not be obtained for this operation")
	}
	if secrets.Protocol != buildSecretsProtocol {
		return errors.New("unsupported build credential protocol")
	}
	return writeBuildSecrets(app, build.SecretNames, secrets.Values)
}

func (d *Docker) privateBuild(ctx context.Context, app string, args ...string) ([]byte, error) {
	args = append(append([]string(nil), args...), "--no-cache")
	cmd := d.dockerCmd(ctx, append([]string{"compose"}, args...)...)
	cmd.Dir = app
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, errors.New("build with private credentials failed; build output was withheld")
	}
	return []byte("Build output withheld because private credentials were mounted."), nil
}
