package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const composeSourceProtocol = "compose-source-files-v1"
const composeSourceMaxBytes = 1024 * 1024

var composeSourcePath = regexp.MustCompile(`^(?:[A-Za-z0-9_][A-Za-z0-9._-]*/){0,9}(?:[A-Za-z0-9_][A-Za-z0-9._-]*|\.env(?:\.[A-Za-z0-9_-]+)?)$`)
var composeSourceHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Runtime bind sources must not follow build-ctx, which changes on redeploy.
// Keep each archive's files immutable so retained releases can replay them.
func stageComposeSourceFiles(appDir string, build *sdkclient.BuildContext) error {
	if build == nil || (len(build.AuxiliaryFiles) == 0 && build.AuxiliaryProtocol == "") {
		return nil
	}
	if build.AuxiliaryProtocol != composeSourceProtocol || build.Git != nil || !composeSourceHash.MatchString(build.SHA256) || len(build.AuxiliaryFiles) == 0 || len(build.AuxiliaryFiles) > 100 {
		return fmt.Errorf("invalid source-file protocol or snapshot")
	}
	files := make(map[string][]byte)
	total := 0
	for _, name := range build.AuxiliaryFiles {
		if len(name) > 240 || !composeSourcePath.MatchString(name) {
			return fmt.Errorf("invalid source-file path")
		}
		for _, part := range strings.Split(name, "/") {
			if part == "node_modules" || part == ".git" {
				return fmt.Errorf("unsupported source-file directory")
			}
		}
		if _, exists := files[name]; exists {
			return fmt.Errorf("duplicate source-file path")
		}
		data, err := readComposeSourceFile(filepath.Join(appDir, "build-ctx"), name)
		if err != nil {
			return err
		}
		total += len(data)
		if total > composeSourceMaxBytes {
			return fmt.Errorf("source files exceed 1 MiB")
		}
		files[name] = data
	}
	root := filepath.Join(appDir, "source-files")
	if err := os.Mkdir(root, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe source-file storage")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	if err := collectComposeSourceFiles(appDir, build.SHA256); err != nil {
		return err
	}
	dest := filepath.Join(root, build.SHA256)
	if info, err := os.Lstat(dest); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe source-file snapshot")
		}
		for name, expected := range files {
			actual, err := readComposeSourceFile(dest, name)
			if err != nil || !bytes.Equal(actual, expected) {
				return fmt.Errorf("immutable source-file snapshot differs")
			}
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	// Unknown state or excessive retained references must not cause data removal.
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) >= 32 {
		return fmt.Errorf("source-file snapshot limit reached; retained releases require review")
	}
	tmp, err := os.MkdirTemp(root, ".staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for name, data := range files {
		path := filepath.Join(tmp, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return err
		}
	}
	return os.Rename(tmp, dest)
}

var composeSourceReference = regexp.MustCompile(`source-files/([a-f0-9]{64})/`)

// Run before replacing compose.yaml, after captureRelease has persisted the
// previous runtime. Retained release JSON embeds its resolved Compose directly.
func collectComposeSourceFiles(appDir, incoming string) error {
	keep := map[string]bool{incoming: true}
	scan := func(path string, optional bool) error {
		info, err := os.Lstat(path)
		if optional && os.IsNotExist(err) {
			return nil
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16*1024*1024 {
			return fmt.Errorf("cannot inspect source-file release references")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("cannot read source-file release references")
		}
		if strings.HasSuffix(path, ".json") && !json.Valid(data) {
			return fmt.Errorf("invalid retained release references")
		}
		for _, match := range composeSourceReference.FindAllSubmatch(data, -1) {
			keep[string(match[1])] = true
		}
		return nil
	}
	if err := scan(filepath.Join(appDir, "compose.yaml"), true); err != nil {
		return err
	}
	releases := filepath.Join(appDir, "releases")
	if info, err := os.Lstat(releases); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe release reference directory")
		}
		entries, err := os.ReadDir(releases)
		if err != nil || len(entries) > 100 {
			return fmt.Errorf("cannot inspect retained releases")
		}
		for _, entry := range entries {
			if !releaseIDPattern.MatchString(strings.TrimSuffix(entry.Name(), ".json")) || !strings.HasSuffix(entry.Name(), ".json") {
				return fmt.Errorf("unknown retained release entry")
			}
			if _, err := loadRelease(appDir, strings.TrimSuffix(entry.Name(), ".json")); err != nil {
				return fmt.Errorf("invalid retained release blocks source-file reclamation")
			}
			if err := scan(filepath.Join(releases, entry.Name()), false); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	root := filepath.Join(appDir, "source-files")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !composeSourceHash.MatchString(entry.Name()) || keep[entry.Name()] {
			continue
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe retained source-file entry")
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("cannot reclaim unused source-file snapshot")
		}
	}
	return nil
}

func readComposeSourceFile(root, name string) ([]byte, error) {
	rootInfo, rootErr := os.Lstat(root)
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("unsafe source-file root")
	}
	path := root
	parts := strings.Split(name, "/")
	for index, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("missing or unsafe source file")
		}
		if index < len(parts)-1 {
			if !info.IsDir() {
				return nil, fmt.Errorf("invalid source-file parent")
			}
		} else if !info.Mode().IsRegular() || info.Size() > composeSourceMaxBytes {
			return nil, fmt.Errorf("invalid or oversized source file")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read source file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, composeSourceMaxBytes+1))
	if err != nil || len(data) > composeSourceMaxBytes {
		return nil, fmt.Errorf("cannot read bounded source file")
	}
	return data, nil
}
