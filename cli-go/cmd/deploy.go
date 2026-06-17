package cmd

// `impreza deploy` — top-level Dockerfile-mode custom deploy.
//
// Phase 12 Iteration 3b. Sibling of `impreza platform deploy <app>`
// (which deploys curated catalog apps); this one is for the
// AI-tool / "I built this myself" path:
//
//	cd ~/my-go-bot
//	impreza deploy --agent agt_xxx --domain bot.example.com
//	  → tars cwd, uploads tarball, posts custom-deploy with
//	    mode=dockerfile, prints the deployment id + poll hint
//
// Defaults
//
//   --name  — basename(cwd) lowercased, non-[a-z0-9_-] replaced with '-'
//   --dockerfile — `Dockerfile` at cwd root
//   --target-port — 80
//   --cpus / --memory-mb — server-side defaults from mod_imprezaapi_config
//                          (1.0 CPU / 512 MB ship)
//
// Exclusions baked into the tarball (always, no config in v1):
//   .git, .svn, .hg, .bzr
//   node_modules, vendor (Go), __pycache__, .venv, venv
//   .DS_Store, Thumbs.db
//   *.pyc
//   .impreza/ (CLI's own state)
//
// We deliberately keep this MINIMAL for v1 — no .dockerignore parser
// yet, no "watch + redeploy on change", no preview deploys. Just the
// happy path so the customer (or an AI tool) can ship an app with
// one command from the project directory.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var (
	deployName       string
	deployAgent      string
	deployDomain     string
	deployOnion      bool
	deployCpus       float64
	deployMemoryMB   int
	deployTargetPort int
	deployEnvFlags   []string
	deployDockerfile string
	deployContextDir string
	deployFollow     bool
	deployForce      bool

	deployGitURL        string
	deployGitRef        string
	deployGitAuthMethod string
	deployGitPat        string
)

var deployCmd = &cobra.Command{
	Use:   "deploy",
	Short: "Build + deploy a Dockerfile in the current directory to one of your servers.",
	Long: `Build + deploy the project in your current directory (or --dir) to
an Impreza-managed server.

The CLI tars + gzips the directory, uploads it to the control plane
as a build context, then asks the server to enqueue a Dockerfile-mode
custom deployment on the target agent. The agent downloads the
tarball, verifies SHA256, extracts, and runs ` + "`docker compose up -d`" + `
— which fires the embedded ` + "`build:`" + ` directive against your Dockerfile.

Typical first run:

  cd ~/my-bot
  impreza deploy --agent agt_xxxxxxxxxxxx --domain bot.example.com

Public images only in v1 (no private-registry auth). Dockerfile builds
that pull from public registries (Docker Hub, ghcr.io public) work
fine; private base images require credentials shipped via --env
secrets (the Dockerfile's RUN docker login).

The deployment id prints on success — track progress with
` + "`impreza platform deployments show <id>`" + ` or ` + "`--follow`" + ` to block
here until status flips out of pending/installing.`,
	Args: cobra.NoArgs,
	RunE: runDeploy,
}

func runDeploy(cmd *cobra.Command, _ []string) error {
	if deployAgent == "" {
		return fmt.Errorf("--agent is required (try: impreza platform servers list)")
	}

	// Git-source mode: clone a repo instead of packaging the local dir.
	if deployGitURL != "" {
		return runGitDeploy(cmd)
	}

	// Resolve the project root.
	projectDir, err := filepath.Abs(deployContextDir)
	if err != nil {
		return fmt.Errorf("resolve project dir: %w", err)
	}
	info, err := os.Stat(projectDir)
	if err != nil {
		return fmt.Errorf("project dir: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("--dir %q is not a directory", projectDir)
	}

	// Resolve Dockerfile path (relative to projectDir).
	dockerfilePath := deployDockerfile
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	dockerfileAbs := filepath.Join(projectDir, dockerfilePath)
	if _, err := os.Stat(dockerfileAbs); err != nil {
		return fmt.Errorf("Dockerfile %q not found in project dir: %w", dockerfilePath, err)
	}

	// Resolve deployment name.
	name := deployName
	if name == "" {
		name = defaultDeployName(projectDir)
	}
	if name == "" {
		return fmt.Errorf("could not derive a deploy name from project dir; pass --name")
	}

	// Parse env flags into vars map.
	vars := map[string]any{}
	for _, kv := range deployEnvFlags {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			return fmt.Errorf("--env must be KEY=VALUE (got %q)", kv)
		}
		vars[kv[:eq]] = kv[eq+1:]
	}

	c, _, err := newClient()
	if err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	// Phase 13 — Stepper output. 4 steps for the no-conflict happy
	// path (Package, Upload, Deploy, Wait); the conflict-replace branch
	// (5 steps total) extends transparently when the server returns 409.
	totalSteps := 3
	if deployFollow {
		totalSteps = 4
	}
	steps := NewStepper(w, totalSteps)

	steps.Step("Packaging project")
	steps.Detail(projectDir)
	tarball, fileCount, err := tarProjectDir(projectDir)
	if err != nil {
		steps.Error("tar project dir failed")
		return fmt.Errorf("tar project dir: %w", err)
	}
	steps.Done(fmt.Sprintf("%d files, %s gzipped", fileCount, humanBytes(int64(len(tarball)))))

	steps.Step("Uploading build context")
	uploadStart := time.Now()
	uploaded, err := c.PlatformUploadCustomDeployContext(cmd.Context(), tarball)
	if err != nil {
		steps.Error("upload failed")
		return fmt.Errorf("upload context: %w", err)
	}
	steps.Done(fmt.Sprintf("%s (sha %s in %.1fs)",
		uploaded.ContextID,
		shortSHA(uploaded.SHA256),
		time.Since(uploadStart).Seconds(),
	))

	steps.Step(fmt.Sprintf("Deploying as %q on %s", name, deployAgent))
	dep, err := submitCustomDeploy(cmd, c, name, uploaded.ContextID, vars, steps)
	if err != nil {
		return err
	}
	steps.Done(fmt.Sprintf("%s (status: %s)", dep.ID, dep.Status))

	if !deployFollow {
		fmt.Fprintf(w, "\nTrack progress with:\n  impreza platform deployments show %s\n", dep.ID)
		return nil
	}

	steps.Step("Waiting for status to settle (--follow)")
	final, err := pollUntilSettled(cmd, c, dep.ID, steps)
	if err != nil {
		return err
	}
	rows := []KV{
		{Key: "id", Value: final.ID},
		{Key: "name", Value: name},
	}
	if final.Domain != "" {
		rows = append(rows, KV{Key: "url", Value: final.Domain})
	}
	if final.Onion != "" {
		rows = append(rows, KV{Key: "onion", Value: final.Onion})
	}
	steps.Banner("✓ Deployed", rows)
	return nil
}

// runGitDeploy handles `impreza deploy --git-url ...` — a Dockerfile-mode
// deploy sourced from a git repo (public, or private via a deploy key or a
// token) instead of the local directory. No local packaging/upload.
func runGitDeploy(cmd *cobra.Command) error {
	method := deployGitAuthMethod
	if method == "" {
		method = "none"
	}
	switch method {
	case "none", "deploy_key", "pat":
	default:
		return fmt.Errorf("--git-auth-method must be none, deploy_key, or pat (got %q)", method)
	}
	if method == "pat" && deployGitPat == "" {
		return fmt.Errorf("--git-pat is required when --git-auth-method=pat")
	}

	vars := map[string]any{}
	for _, kv := range deployEnvFlags {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			return fmt.Errorf("--env must be KEY=VALUE (got %q)", kv)
		}
		vars[kv[:eq]] = kv[eq+1:]
	}

	name := deployName
	if name == "" {
		name = defaultDeployNameFromGit(deployGitURL)
	}
	if name == "" {
		return fmt.Errorf("could not derive a deploy name from --git-url; pass --name")
	}

	c, _, err := newClient()
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()

	ref := deployGitRef
	if ref == "" {
		ref = "main"
	}

	total := 1
	if deployFollow {
		total = 2
	}
	steps := NewStepper(w, total)
	steps.Step(fmt.Sprintf("Deploying %q from git on %s", name, deployAgent))
	steps.Detail(fmt.Sprintf("%s @ %s (auth: %s)", deployGitURL, ref, method))
	dep, err := submitCustomDeploy(cmd, c, name, "", vars, steps)
	if err != nil {
		return err
	}
	steps.Done(fmt.Sprintf("%s (status: %s)", dep.ID, dep.Status))

	// deploy_key: surface the generated public key to add to the repo.
	if dep.GitAuth != nil && dep.GitAuth.Method == "deploy_key" && dep.GitAuth.PublicKey != "" {
		fmt.Fprintf(w, "\nAdd this as a read-only Deploy Key on your repository, then redeploy:\n\n  %s\n", dep.GitAuth.PublicKey)
	}

	if !deployFollow {
		fmt.Fprintf(w, "\nTrack progress with:\n  impreza platform deployments show %s\n", dep.ID)
		return nil
	}

	steps.Step("Waiting for status to settle (--follow)")
	final, err := pollUntilSettled(cmd, c, dep.ID, steps)
	if err != nil {
		return err
	}
	rows := []KV{
		{Key: "id", Value: final.ID},
		{Key: "name", Value: name},
	}
	if final.Domain != "" {
		rows = append(rows, KV{Key: "url", Value: final.Domain})
	}
	if final.Onion != "" {
		rows = append(rows, KV{Key: "onion", Value: final.Onion})
	}
	steps.Banner("✓ Deployed", rows)
	return nil
}

// defaultDeployNameFromGit derives a slug-safe deploy name from a git URL's
// last path segment ("git@github.com:you/My-Repo.git" → "my-repo"), or ""
// if nothing usable remains (caller must pass --name). Same charset rule as
// defaultDeployName.
func defaultDeployNameFromGit(gitURL string) string {
	s := strings.TrimSuffix(strings.TrimSpace(gitURL), ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.ToLower(s)
	s = nonNameRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_")
	if len(s) < 3 {
		return ""
	}
	return s
}

// submitCustomDeploy wraps PlatformCreateCustomDeployment + handles
// the per-customer name-conflict case (server returns 409 CONFLICT
// when the customer re-runs `impreza deploy` against an existing
// deploy of the same name). Behavior:
//
//   - interactive TTY + no --force: prompts "Replace existing? [y/N]".
//     Y → POST uninstall, poll for uninstalled status, retry create.
//     N → returns the original 409 error so the caller exits non-zero.
//   - non-TTY OR --force: skips the prompt + auto-replaces.
//   - any other error: passes through unchanged.
//
// Returns the created deployment OR an error wrapped from the server.
func submitCustomDeploy(
	cmd *cobra.Command,
	c *sdkclient.Client,
	name, contextID string,
	vars map[string]any,
	steps *Stepper,
) (*sdkclient.CustomDeployment, error) {
	req := sdkclient.CustomDeployRequest{
		Name:       name,
		AgentID:    deployAgent,
		Domain:     deployDomain,
		Onion:      deployOnion,
		Vars:       vars,
		Cpus:       deployCpus,
		MemoryMB:   deployMemoryMB,
		TargetPort: deployTargetPort,
		ContextID:  contextID,
	}
	if deployDockerfile != "" && deployDockerfile != "Dockerfile" {
		req.DockerfilePath = deployDockerfile
	}
	// Git-source mode (no local context) — clone instead of a tarball.
	if deployGitURL != "" {
		req.ContextID = ""
		req.GitURL = deployGitURL
		req.GitRef = deployGitRef
		req.GitAuthMethod = deployGitAuthMethod
		req.GitPat = deployGitPat
	}

	dep, err := c.PlatformCreateCustomDeployment(cmd.Context(), req)
	if err == nil {
		return dep, nil
	}
	if !isConflictErr(err) {
		return nil, fmt.Errorf("create deploy: %w", err)
	}

	// Conflict — same per-customer name already deployed. Offer to
	// replace by uninstalling + re-creating with the same context_id.
	steps.Warn(fmt.Sprintf("a deployment named %q already exists on this account", name))

	replace := deployForce
	if !replace {
		if !isStdinTTY() {
			return nil, fmt.Errorf("%w\n  Re-run with --force to replace, OR uninstall the existing one first", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "    Replace it? [y/N]: ")
		in := bufio.NewReader(os.Stdin)
		line, rerr := in.ReadString('\n')
		if rerr != nil {
			return nil, fmt.Errorf("read confirmation: %w", rerr)
		}
		ans := strings.ToLower(strings.TrimSpace(line))
		if ans != "y" && ans != "yes" {
			return nil, fmt.Errorf("aborted by user (pass --force to skip this prompt)")
		}
		replace = true
	}

	// Find the existing deployment_id by listing custom deployments
	// filtered to the target agent + matching name.
	steps.Detail("looking up existing deployment id ...")
	list, lerr := c.PlatformListCustomDeployments(cmd.Context(), deployAgent)
	if lerr != nil {
		return nil, fmt.Errorf("list deployments for replace: %w", lerr)
	}
	var oldID string
	for _, d := range list.Deployments {
		if d.Name == name {
			oldID = d.ID
			break
		}
	}
	if oldID == "" {
		// The list filter on agent_id may have hidden it (different
		// agent). Last-ditch: full list across agents.
		listAll, lerr := c.PlatformListCustomDeployments(cmd.Context(), "")
		if lerr == nil {
			for _, d := range listAll.Deployments {
				if d.Name == name {
					oldID = d.ID
					break
				}
			}
		}
	}
	if oldID == "" {
		return nil, fmt.Errorf("server reported conflict but we couldn't locate the existing %q to replace; please uninstall it manually", name)
	}

	steps.Detail(fmt.Sprintf("uninstalling %s ...", oldID))
	if _, uerr := c.PlatformUninstall(cmd.Context(), oldID, sdkclient.PlatformUninstallRequest{
		PurgeData: false,
		Confirm:   true,
	}); uerr != nil {
		return nil, fmt.Errorf("uninstall existing %s: %w", oldID, uerr)
	}

	// Poll until status flips to uninstalled (or budget expires). The
	// server returns 202 immediately + the agent does the real teardown
	// on its next poll. ~60s is enough for compose down on a healthy
	// agent; longer + we surface as a failure so the customer can
	// retry instead of hanging.
	uninstDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(uninstDeadline) {
		time.Sleep(3 * time.Second)
		cur, gerr := c.PlatformGetCustomDeployment(cmd.Context(), oldID)
		if gerr != nil {
			// Transient — keep polling.
			continue
		}
		if cur.Status == sdkclient.DeploymentUninstalled {
			break
		}
		if cur.Status == sdkclient.DeploymentFailed {
			return nil, fmt.Errorf("replace: uninstall of %s ended in failed state", oldID)
		}
	}
	steps.Done(fmt.Sprintf("replaced %s", oldID))

	// Retry create. Context_id is unconsumed only if the original
	// create rejected BEFORE consumption (server's 409 path runs before
	// the consume-mark UPDATE) — re-use is safe.
	dep, err = c.PlatformCreateCustomDeployment(cmd.Context(), req)
	if err != nil {
		return nil, fmt.Errorf("re-create after replace: %w", err)
	}
	return dep, nil
}

// pollUntilSettled blocks until the deployment leaves pending/installing
// or the budget expires. Returns the final CustomDeployment + nil on
// success, or an error on failure / timeout.
func pollUntilSettled(
	cmd *cobra.Command,
	c *sdkclient.Client,
	id string,
	steps *Stepper,
) (*sdkclient.CustomDeployment, error) {
	deadline := time.Now().Add(10 * time.Minute)
	lastStatus := ""
	for time.Now().Before(deadline) {
		select {
		case <-cmd.Context().Done():
			return nil, cmd.Context().Err()
		case <-time.After(5 * time.Second):
		}
		cur, err := c.PlatformGetCustomDeployment(cmd.Context(), id)
		if err != nil {
			steps.Warn(fmt.Sprintf("poll error (will retry): %v", err))
			continue
		}
		if string(cur.Status) != lastStatus {
			steps.Detail("status: " + string(cur.Status))
			lastStatus = string(cur.Status)
		}
		switch cur.Status {
		case sdkclient.DeploymentRunning:
			return cur, nil
		case sdkclient.DeploymentFailed:
			if cur.LastError != "" {
				return nil, fmt.Errorf("deploy failed: %s", cur.LastError)
			}
			return nil, fmt.Errorf("deploy failed (no error reported)")
		}
	}
	return nil, fmt.Errorf("timed out after 10 minutes; check `impreza platform deployments show %s`", id)
}

// isConflictErr matches the server's 409 CONFLICT envelope for a
// per-customer name collision on custom deploys. The SDK returns these
// as generic errors with the code prefix wrapped in — match by
// substring rather than exporting a typed error from the SDK (which
// would be a churn).
func isConflictErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "CONFLICT") || strings.Contains(msg, "already exists")
}

// defaultDeployName returns a slug-safe per-customer name derived
// from the project directory's basename. "MyBot" → "mybot",
// "my bot" → "my-bot", "." (cwd at /) → "" (caller must pass --name).
//
// The server enforces: ^[a-z0-9][a-z0-9_-]{1,98}[a-z0-9]$ (3-100 chars).
// We pad single-char basenames so the regex's min-3 requirement passes
// without forcing the customer to retype.
func defaultDeployName(projectDir string) string {
	base := filepath.Base(projectDir)
	if base == "." || base == "/" || base == "\\" {
		return ""
	}
	s := strings.ToLower(base)
	s = nonNameRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_")
	if len(s) < 3 {
		if len(s) == 0 {
			return ""
		}
		s = s + strings.Repeat("0", 3-len(s))
	}
	if len(s) > 100 {
		s = s[:100]
	}
	return s
}

var nonNameRE = regexp.MustCompile(`[^a-z0-9_-]`)

// excludedDirs is the bake-in set of project paths we never include
// in the tarball. v1 keeps this minimal + non-configurable — a
// real .dockerignore parser ships in a later iteration.
var excludedDirs = map[string]bool{
	".git":         true,
	".svn":         true,
	".hg":          true,
	".bzr":         true,
	"node_modules": true,
	"__pycache__":  true,
	".venv":        true,
	"venv":         true,
	".impreza":     true,
}

var excludedFiles = map[string]bool{
	".DS_Store": true,
	"Thumbs.db": true,
}

func isExcluded(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, p := range parts {
		if excludedDirs[p] {
			return true
		}
	}
	base := filepath.Base(rel)
	if excludedFiles[base] {
		return true
	}
	if strings.HasSuffix(base, ".pyc") {
		return true
	}
	return false
}

// tarProjectDir gzip-tars projectDir + returns the bytes + a file
// count. Symlinks are followed by-default (most customers expect
// "deploy what I see" semantics; the server-side 100 MB cap is the
// safety net for runaway link chasing).
func tarProjectDir(projectDir string) ([]byte, int, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	fileCount := 0
	err := filepath.Walk(projectDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(projectDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if isExcluded(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			fh, err := os.Open(path)
			if err != nil {
				return err
			}
			n, err := io.Copy(tw, fh)
			_ = fh.Close()
			if err != nil {
				return err
			}
			if n != info.Size() {
				return fmt.Errorf("short read on %s: wrote %d of %d", rel, n, info.Size())
			}
			fileCount++
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), fileCount, nil
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(k))
	case n < k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(k*k))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(k*k*k))
	}
}

func shortSHA(sha string) string {
	if len(sha) >= 12 {
		return sha[:12]
	}
	return sha
}

func init() {
	deployCmd.Flags().StringVar(&deployName, "name", "",
		"Deploy name (per-account unique). Default: project dir basename.")
	deployCmd.Flags().StringVar(&deployAgent, "agent", "",
		"agent_id of the target server (required).")
	deployCmd.Flags().StringVar(&deployDomain, "domain", "",
		"Public hostname. Omit when --onion is set for an onion-only deploy.")
	deployCmd.Flags().BoolVar(&deployOnion, "onion", false,
		"Publish a Tor v3 hidden service mirror.")
	deployCmd.Flags().Float64Var(&deployCpus, "cpus", 0,
		"CPU limit (cores, decimal OK). Default: server-side (1.0).")
	deployCmd.Flags().IntVar(&deployMemoryMB, "memory-mb", 0,
		"Memory limit in MB. Default: server-side (512).")
	deployCmd.Flags().IntVar(&deployTargetPort, "target-port", 80,
		"Port the container listens on internally.")
	deployCmd.Flags().StringArrayVar(&deployEnvFlags, "env", nil,
		"KEY=VALUE env var for the container (repeatable).")
	deployCmd.Flags().StringVar(&deployDockerfile, "dockerfile", "",
		"Path to Dockerfile relative to project dir. Default: Dockerfile.")
	deployCmd.Flags().StringVar(&deployContextDir, "dir", ".",
		"Project directory to package + deploy. Default: cwd.")
	deployCmd.Flags().StringVar(&deployGitURL, "git-url", "",
		"Deploy from a git repo instead of the local dir. https (public, or private with --git-auth-method=pat), or SSH like git@github.com:owner/repo.git (--git-auth-method=deploy_key).")
	deployCmd.Flags().StringVar(&deployGitRef, "git-ref", "",
		"Branch / tag / commit to clone with --git-url. Default: main.")
	deployCmd.Flags().StringVar(&deployGitAuthMethod, "git-auth-method", "",
		"Private-repo auth for --git-url: none (default), deploy_key (SSH), or pat (token).")
	deployCmd.Flags().StringVar(&deployGitPat, "git-pat", "",
		"Fine-grained, repo-scoped, Contents:Read token (required with --git-auth-method=pat).")
	deployCmd.Flags().BoolVar(&deployFollow, "follow", false,
		"Block until the deployment leaves pending/installing.")
	deployCmd.Flags().BoolVar(&deployForce, "force", false,
		"When the deploy `name` is already in use on this account, uninstall the existing one + create new (no confirmation prompt).")

	_ = deployCmd.MarkFlagRequired("agent")

	rootCmd.AddCommand(deployCmd)
}
