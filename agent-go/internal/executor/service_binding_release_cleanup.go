package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const retiredReleaseBindingMessage = "release changes a managed database connection; review the connection before redeploying; containers were not changed"

var retainedBindingURLs = regexp.MustCompile(postgresBindingURL + "|" + mysqlBindingURL)

// Only a verified retirement calls this phase. Keep the archive (and immutable
// image identities), but make its revoked credential unavailable for replay.
func (d *Docker) redactRetainedBindingCredentials(ctx context.Context, consumer string, retired sdkclient.ServiceBindingRetirement) error {
	fail := errors.New("retained database credential cleanup could not be completed")
	if validateServiceBindingRef(consumer, retired.ServiceBindingRef) != nil {
		return fail
	}
	_, _, login := bindingGenerationNames(retired.ServiceBindingRef)
	if retired.Username != login && retired.Username != "imp_"+strings.TrimPrefix(retired.BindingID, "bnd_") {
		return fail
	}
	archive := filepath.Join(d.appDir(consumer), "releases")
	info, err := os.Lstat(archive)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fail
	}
	passwords := func(raw []byte) map[string]string {
		found := map[string]string{}
		for _, value := range retainedBindingURLs.FindAllString(string(raw), -1) {
			u, err := url.Parse(value)
			if err != nil || u.User == nil || u.User.Username() != retired.Username {
				continue
			}
			if u.Host != "pg_"+retired.ProviderDeploymentID+":5432" && u.Host != "mariadb_"+retired.ProviderDeploymentID+":3306" {
				continue
			}
			password, ok := u.User.Password()
			if ok {
				found[password] = password
			}
		}
		return found
	}
	current, err := d.compose(ctx, d.appDir(consumer), "config", "--format", "json")
	if err != nil || len(passwords(current)) != 0 {
		return fail
	}
	files, err := os.ReadDir(archive)
	if err != nil {
		return fail
	}
	for _, entry := range files {
		if !strings.HasPrefix(entry.Name(), "rel_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !releaseIDPattern.MatchString(id) {
			return fail
		}
		path := filepath.Join(archive, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16*1024*1024 {
			return fail
		}
		f, err := os.Open(path)
		if err != nil {
			return fail
		}
		actual, statErr := f.Stat()
		raw, readErr := io.ReadAll(io.LimitReader(f, 16*1024*1024+1))
		closeErr := f.Close()
		if statErr != nil || !os.SameFile(info, actual) || readErr != nil || closeErr != nil || len(raw) > 16*1024*1024 {
			return fail
		}
		var release runtimeRelease
		if json.Unmarshal(raw, &release) != nil || release.Metadata.ID != id {
			return fail
		}
		values := passwords(release.Compose)
		for k, v := range passwords(release.Env) {
			values[k] = v
		}
		if len(values) == 0 {
			continue
		}
		release.Compose = json.RawMessage(redactServiceBindingText(string(release.Compose), values))
		release.Env = []byte(redactServiceBindingText(string(release.Env), values))
		release.RetiredBindingCredential = true
		encoded, err := json.Marshal(release)
		if err != nil {
			return fail
		}
		// The temporary contains only the sanitized version. writeAtomic removes it
		// on all failure paths; the previous snapshot survives a failed replacement.
		if writeAtomic(path, encoded, 0600) != nil {
			return fail
		}
	}
	return nil
}
