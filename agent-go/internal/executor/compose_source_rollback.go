package executor

import (
	"fmt"
	"path/filepath"
	"strings"
)

// A versioned config may change contents, but never its resource identity,
// mount destination, or archive-relative filename during an image rollback.
func releaseSourceContract(raw []byte, appDir string) (map[string]any, error) {
	contract, err := releaseContract(raw)
	if err != nil {
		return nil, err
	}
	prefix := filepath.ToSlash(filepath.Join(appDir, "source-files")) + "/"
	for _, kind := range []string{"configs", "secrets"} {
		resources, _ := contract[kind].(map[string]any)
		for _, value := range resources {
			resource, ok := value.(map[string]any)
			if !ok {
				continue
			}
			file, ok := resource["file"].(string)
			if !ok || !strings.HasPrefix(file, prefix) {
				continue
			}
			parts := strings.SplitN(strings.TrimPrefix(file, prefix), "/", 2)
			if len(parts) != 2 || !composeSourceHash.MatchString(parts[0]) || !composeSourcePath.MatchString(parts[1]) {
				return nil, fmt.Errorf("invalid versioned release file")
			}
			if _, err := readComposeSourceFile(filepath.Join(appDir, "source-files", parts[0]), parts[1]); err != nil {
				return nil, fmt.Errorf("versioned release file unavailable")
			}
			resource["file"] = prefix + "<retained-version>/" + parts[1]
		}
	}
	return contract, nil
}
