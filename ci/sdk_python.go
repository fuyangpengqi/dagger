package main

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/dagger/dagger/ci/internal/dagger"
	"github.com/dagger/dagger/ci/util"
)

// TODO: use dev module (this is just the mage port)

const (
	pythonSubdir           = "sdk/python"
	pythonRuntimeSubdir    = "sdk/python/runtime"
	pythonGeneratedAPIPath = "sdk/python/src/dagger/client/gen.py"
	pythonDefaultVersion   = "3.11"
)

var (
	pythonVersions = []string{"3.10", "3.11", "3.12"}
)

type PythonSDK struct {
	Dagger *Dagger // +private
}

// Lint the Python SDK
func (t PythonSDK) Lint(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)

	base := t.pythonBase(pythonDefaultVersion, true)

	eg.Go(func() error {
		path := "docs/current_docs"
		_, err := base.
			WithDirectory(
				fmt.Sprintf("/%s", path),
				t.Dagger.Source.Directory(path),
				dagger.ContainerWithDirectoryOpts{
					Include: []string{
						"**/*.py",
						".ruff.toml",
					},
				},
			).
			WithExec([]string{"ruff", "check", "--show-source", ".", "/docs"}).
			WithExec([]string{"black", "--check", "--diff", ".", "/docs"}).
			Sync(ctx)
		return err
	})

	eg.Go(func() error {
		return util.DiffDirectoryF(ctx, t.Dagger.Source, t.Generate, pythonGeneratedAPIPath)
	})

	eg.Go(func() error {
		return lintGoModule(ctx, false, daggerDevelop(t.Dagger.Source, pythonRuntimeSubdir), []string{pythonRuntimeSubdir})
	})

	return eg.Wait()
}

// Test the Python SDK
func (t PythonSDK) Test(ctx context.Context) error {
	installer, err := t.Dagger.installer(ctx, "sdk-python-test")
	if err != nil {
		return err
	}

	eg, ctx := errgroup.WithContext(ctx)
	for _, version := range pythonVersions {
		base := t.pythonBase(version, true).With(installer)

		eg.Go(func() error {
			_, err := base.
				WithEnvVariable("PYTHONUNBUFFERED", "1").
				WithExec([]string{"pytest", "-Wd", "--exitfirst", "-m", "not provision"}).
				Sync(ctx)
			return err
		})

		// Test build
		dist := t.pythonBase(version, false).
			WithMountedDirectory(
				"/dist",
				base.
					WithExec([]string{"hatch", "build", "--clean"}).
					Directory("dist"),
			)

		for _, ext := range map[string]string{"sdist": "tar.gz", "bdist": "whl"} {
			ext := ext
			eg.Go(func() error {
				_, err := dist.
					WithExec([]string{"sh", "-c", "pip install /dist/*" + ext}).
					WithExec([]string{"python", "-c", "import dagger"}).
					Sync(ctx)
				return err
			})
		}
	}

	return eg.Wait()
}

// Regenerate the Python SDK API
func (t PythonSDK) Generate(ctx context.Context) (*dagger.Directory, error) {
	installer, err := t.Dagger.installer(ctx, "sdk-python-generate")
	if err != nil {
		return nil, err
	}
	introspection, err := t.Dagger.introspection(ctx, installer)
	if err != nil {
		return nil, err
	}
	generated := t.pythonBase(pythonDefaultVersion, true).
		// codegen lock file has a relative `-e .` path
		WithWorkdir("./codegen").
		WithExec([]string{"pip", "install", "-r", "requirements.lock"}).
		WithMountedFile("/schema.json", introspection).
		WithExec([]string{"python", "-m", "codegen", "generate", "-i", "/schema.json", "-o", "gen.py"}).
		WithExec([]string{"black", "gen.py"}).
		File("gen.py")
	return dag.Directory().WithFile(pythonGeneratedAPIPath, generated), nil
}

// Publish the Python SDK
func (t PythonSDK) Publish(
	ctx context.Context,
	tag string,

	// +optional
	dryRun bool,

	// +optional
	pypiRepo string,
	// +optional
	pypiToken *Secret,
) error {
	version := strings.TrimPrefix(tag, "sdk/python/v")
	if pypiRepo == "" || pypiRepo == "pypi" {
		pypiRepo = "main"
	}

	result := t.pythonBase(pythonDefaultVersion, true).
		WithEnvVariable("SETUPTOOLS_SCM_PRETEND_VERSION", version).
		WithEnvVariable("HATCH_INDEX_REPO", pypiRepo).
		WithEnvVariable("HATCH_INDEX_USER", "__token__").
		WithExec([]string{"hatch", "build"})
	if !dryRun {
		result = result.
			WithSecretVariable("HATCH_INDEX_AUTH", pypiToken).
			WithExec([]string{"hatch", "publish"})
	}
	_, err := result.Sync(ctx)
	return err
}

// Bump the Python SDK's Engine dependency
func (t PythonSDK) Bump(ctx context.Context, version string) (*dagger.Directory, error) {
	// trim leading v from version
	version = strings.TrimPrefix(version, "v")
	engineReference := fmt.Sprintf("# Code generated by dagger. DO NOT EDIT.\n\nCLI_VERSION = %q\n", version)

	// NOTE: if you change this path, be sure to update .github/workflows/publish.yml so that
	// provision tests run whenever this file changes.
	return dag.Directory().WithNewFile("sdk/python/src/dagger/_engine/_version.py", engineReference), nil
}

// pythonBase returns a python container with the Python SDK source files
// added and dependencies installed.
func (t PythonSDK) pythonBase(version string, install bool) *Container {
	src := t.Dagger.Source.Directory(pythonSubdir)

	pipx := dag.HTTP("https://github.com/pypa/pipx/releases/download/1.2.0/pipx.pyz")
	venv := "/opt/venv"

	base := dag.Container().
		From(fmt.Sprintf("python:%s-slim", version)).
		WithEnvVariable("PIPX_BIN_DIR", "/usr/local/bin").
		WithMountedCache("/root/.cache/pip", dag.CacheVolume("pip_cache_"+version)).
		WithMountedCache("/root/.local/pipx/cache", dag.CacheVolume("pipx_cache_"+version)).
		WithMountedCache("/root/.cache/hatch", dag.CacheVolume("hatch_cache_"+version)).
		WithMountedFile("/pipx.pyz", pipx).
		WithExec([]string{"python", "/pipx.pyz", "install", "hatch==1.7.0"}).
		WithExec([]string{"python", "-m", "venv", venv}).
		WithEnvVariable("VIRTUAL_ENV", venv).
		WithEnvVariable(
			"PATH",
			"$VIRTUAL_ENV/bin:$PATH",
			dagger.ContainerWithEnvVariableOpts{
				Expand: true,
			},
		).
		WithEnvVariable("HATCH_ENV_TYPE_VIRTUAL_PATH", venv).
		// Mirror the same dir structure from the repo because of the
		// relative paths in ruff (for docs linting).
		WithWorkdir(pythonSubdir).
		WithMountedFile("requirements.txt", src.File("requirements.txt")).
		WithExec([]string{"pip", "install", "-r", "requirements.txt"})

	if install {
		base = base.
			WithMountedDirectory("", src).
			WithExec([]string{"pip", "install", "--no-deps", "."})
	}

	return base
}
