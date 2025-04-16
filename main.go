package main

import (
	"context"
	"io/fs"
	"strings"

	"fmt"
	"github.com/pelletier/go-toml"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"

	"golang.org/x/oauth2"

	"github.com/google/go-github/v68/github"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		cancel()
		<-sigs
		os.Exit(130)
	}()

	err := run(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	src, err := downloadBuildpackSource(ctx)
	if err != nil {
		return fmt.Errorf("cannot get buildpack source: %w", err)
	}

	versions, err := getVersions(src)
	if err != nil {
		return fmt.Errorf("cannot get versions: %w", err)
	}

	compiledVersions, err := getCompiledVersions(ctx)
	if err != nil {
		return fmt.Errorf("cannot get compiled versions: %w", err)
	}

	versionsToCompile := make([]string, 0, len(versions))
	for v, _ := range versions {
		if _, ok := compiledVersions[v]; !ok {
			versionsToCompile = append(versionsToCompile, v)
		}
	}
	if len(versionsToCompile) == 0 {
		fmt.Println("all required versions are already built")
		return nil
	}

	builderImage := "compilation"
	cmd := exec.CommandContext(ctx, "docker",
		"build",
		filepath.Join(src, "dependency/actions/compile"),
		"-t", builderImage,
		"-f", filepath.Join(src, "dependency/actions/compile/jammy.Dockerfile"),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "BUILDKIT_PROGRESS=plain")
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("cannot build builder image: %w", err)
	}

	out, err := os.MkdirTemp("", "")
	if err != nil {
		return fmt.Errorf("cannot create temp for artifacts: %w", err)
	}

	for _, v := range versionsToCompile {
		if _, ok := compiledVersions[v]; ok {
			continue
		}
		cmd = exec.CommandContext(ctx, "docker",
			"run", fmt.Sprintf("-v%s:%s", out, "/home"),
			builderImage,
			"--version", v,
			"--outputDir", "/home",
			"--target", "jammy",
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "BUILDKIT_PROGRESS=plain")
		err = cmd.Run()
		if err != nil {
			return fmt.Errorf("cannot build cpython: %w", err)
		}
	}
	err = filepath.Walk(out, func(p string, fi fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".tgz") || strings.HasSuffix(p, ".tgz.checksum") {
			err = uploadAsset(ctx, p)
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("error while processing artifacts: %w", err)
	}
	return nil
}

func downloadBuildpackSource(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://github.com/paketo-buildpacks/cpython/archive/refs/heads/main.tar.gz", nil)
	if err != nil {
		return "", fmt.Errorf("cannot create http request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot get")
	}
	d, err := os.MkdirTemp("", "")
	if err != nil {
		return "", fmt.Errorf("cannot create temp dir for source code: %w", err)
	}
	cmd := exec.CommandContext(ctx, "tar",
		"xzf", "-",
		"-C", d,
		"--strip-components=1")
	cmd.Stdin = resp.Body
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		return "", fmt.Errorf("cannot extract sources: %w", err)
	}
	return d, nil
}

func getVersions(src string) (map[string]struct{}, error) {
	f, err := os.Open(filepath.Join(src, "buildpack.toml"))
	if err != nil {
		return nil, fmt.Errorf("cannot open buildpack.toml: %w", err)
	}
	versions := make(map[string]struct{})
	dec := toml.NewDecoder(f)
	m := data{}
	err = dec.Decode(&m)
	if err != nil {
		return nil, fmt.Errorf("cannot decode buildpack.toml: %w", err)
	}
	for _, d := range m.Metadata.Dependencies {
		versions[d.Version] = struct{}{}
	}
	return versions, nil
}

type data struct {
	Metadata struct {
		Dependencies []struct {
			Version string
		}
	}
}

func getCompiledVersions(ctx context.Context) (map[string]struct{}, error) {
	cli := newGHClient(ctx)

	owner := "matejvasek"
	repo := "cpython-dist"

	rel, resp, err := cli.Repositories.GetReleaseByTag(ctx, owner, repo, "v0.0.0")
	if err != nil {
		return nil, fmt.Errorf("cannot list releases: %w", err)
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	versions := make(map[string]struct{}, len(rel.Assets))

	r := regexp.MustCompile(`python_(\d+\.\d+\.\d+)_linux_arm64`)
	for _, a := range rel.Assets {
		matches := r.FindStringSubmatch(a.GetName())
		if len(matches) != 2 {
			continue
		}
		versions[matches[1]] = struct{}{}
	}
	return versions, nil
}

func uploadAsset(ctx context.Context, p string) error {

	name := filepath.Base(p)
	name = strings.ReplaceAll(name, "_x64_", "_arm64_")

	r := regexp.MustCompile(`_[a-fA-F0-9]{8}.`)

	name = r.ReplaceAllString(name, ".")
	var mediaType string
	switch {
	case strings.HasSuffix(name, ".tgz"):
		mediaType = "application/gzip"
	case strings.HasSuffix(name, ".sha256"):
	case strings.HasSuffix(name, ".checksum"):
		mediaType = "text/plain"
	default:
		mediaType = "application/octet-stream"
	}

	var uploadOptions = &github.UploadOptions{
		Name:      name,
		MediaType: mediaType,
	}
	fmt.Printf("UPLOAD: %+v\n", uploadOptions)

	cli := newGHClient(ctx)

	owner := "matejvasek"
	repo := "cpython-dist"

	rel, resp, err := cli.Repositories.GetReleaseByTag(ctx, owner, repo, "v0.0.0")
	if err != nil {
		return fmt.Errorf("cannot list releases: %w", err)
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("cannot open file: %w", err)
	}

	_, resp, err = cli.Repositories.UploadReleaseAsset(ctx, owner, repo, rel.GetID(), uploadOptions, f)
	if err != nil {
		return fmt.Errorf("cannot upload asset: %w", err)
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	return nil
}

func newGHClient(ctx context.Context) *github.Client {
	return github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: os.Getenv("GITHUB_TOKEN"),
	})))
}
