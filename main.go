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

	versions, err := getVersions(src)
	if err != nil {
		return fmt.Errorf("cannot get versions: %w", err)
	}

	compiledVersions, err := getCompiledVersions(ctx)
	if err != nil {
		return fmt.Errorf("cannot get compiled versions: %w", err)
	}

	out, err := os.MkdirTemp("", "")
	if err != nil {
		return fmt.Errorf("cannot create temp for artifacts: %w", err)
	}

	for v := range versions {
		if _, ok := compiledVersions[v]; ok {
			fmt.Println("already present:", v)
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
		if strings.HasSuffix(p, ".tgz") {
			fmt.Println("will upload:", p)
			err = uploadTGZ(ctx, p)
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
		"xzvf", "-",
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

func uploadTGZ(ctx context.Context, p string) error {
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

	name := filepath.Base(f.Name())
	name = strings.ReplaceAll(name, "_x64_", "_arm64_")
	var uploadOptions = &github.UploadOptions{
		Name:      name,
		MediaType: "application/tgz",
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
