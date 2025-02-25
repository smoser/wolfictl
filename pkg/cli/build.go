package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"chainguard.dev/apko/pkg/build/types"
	"chainguard.dev/melange/pkg/build"
	"chainguard.dev/melange/pkg/config"
	"chainguard.dev/melange/pkg/container"
	"chainguard.dev/melange/pkg/container/docker"
	"chainguard.dev/melange/pkg/index"
	"github.com/chainguard-dev/clog"
	charmlog "github.com/charmbracelet/log"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/trace"
	"golang.org/x/exp/maps"
	"golang.org/x/sync/errgroup"

	"github.com/wolfi-dev/wolfictl/pkg/dag"
)

func cmdBuild() *cobra.Command {
	var jobs int
	var traceFile string

	cfg := global{}

	// TODO: buildworld bool (build deps vs get them from package repo)
	// TODO: builddownstream bool (build things that depend on listed packages)
	cmd := &cobra.Command{
		Use:           "build",
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			if traceFile != "" {
				w, err := os.Create(traceFile)
				if err != nil {
					return fmt.Errorf("creating trace file: %w", err)
				}
				defer w.Close()
				exporter, err := stdouttrace.New(stdouttrace.WithWriter(w))
				if err != nil {
					return fmt.Errorf("creating stdout exporter: %w", err)
				}
				tp := trace.NewTracerProvider(trace.WithBatcher(exporter))
				otel.SetTracerProvider(tp)

				defer func() {
					if err := tp.Shutdown(context.WithoutCancel(ctx)); err != nil {
						clog.FromContext(ctx).Errorf("Shutting down trace provider: %v", err)
					}
				}()

				tctx, span := otel.Tracer("wolfictl").Start(ctx, "build")
				defer span.End()
				ctx = tctx
			}

			if jobs == 0 {
				jobs = runtime.GOMAXPROCS(0)
			}
			cfg.jobch = make(chan struct{}, jobs)
			cfg.donech = make(chan *task, jobs)

			if cfg.signingKey == "" {
				cfg.signingKey = filepath.Join(cfg.dir, "local-melange.rsa")
			}
			if cfg.pipelineDir == "" {
				cfg.pipelineDir = filepath.Join(cfg.dir, "pipelines")
			}
			if cfg.outDir == "" {
				cfg.outDir = filepath.Join(cfg.dir, "packages")
			}

			var eg errgroup.Group

			// Logs will go here to mimic the wolfi Makefile.
			for _, arch := range cfg.archs {
				arch := arch
				eg.Go(func() error {
					return buildArch(ctx, &cfg, arch, args)
				})
			}

			return eg.Wait()
		},
	}

	cmd.Flags().StringVarP(&cfg.dir, "dir", "d", ".", "directory to search for melange configs")
	cmd.Flags().StringVar(&cfg.pipelineDir, "pipeline-dir", "", "directory used to extend defined built-in pipelines")
	cmd.Flags().StringVar(&cfg.runner, "runner", "docker", "which runner to use to enable running commands, default is based on your platform.")
	cmd.Flags().StringSliceVar(&cfg.archs, "arch", []string{"x86_64", "aarch64"}, "arch of package to build")
	cmd.Flags().BoolVar(&cfg.dryrun, "dry-run", false, "print commands instead of executing them")
	cmd.Flags().StringSliceVarP(&cfg.extraKeys, "keyring-append", "k", []string{"https://packages.wolfi.dev/os/wolfi-signing.rsa.pub"}, "path to extra keys to include in the build environment keyring")
	cmd.Flags().StringSliceVarP(&cfg.extraRepos, "repository-append", "r", []string{"https://packages.wolfi.dev/os"}, "path to extra repositories to include in the build environment")
	cmd.Flags().StringVar(&cfg.signingKey, "signing-key", "", "key to use for signing")
	cmd.Flags().StringVar(&cfg.namespace, "namespace", "wolfi", "namespace to use in package URLs in SBOM (eg wolfi, alpine)")
	cmd.Flags().StringVar(&cfg.outDir, "out-dir", "", "directory where packages will be output")
	cmd.Flags().StringVar(&cfg.cacheDir, "cache-dir", "./melange-cache/", "directory used for cached inputs")
	cmd.Flags().StringVar(&cfg.cacheSource, "cache-source", "", "directory or bucket used for preloading the cache")
	cmd.Flags().BoolVar(&cfg.generateIndex, "generate-index", true, "whether to generate APKINDEX.tar.gz")

	cmd.Flags().IntVarP(&jobs, "jobs", "j", 0, "number of jobs to run concurrently (default is GOMAXPROCS)")
	cmd.Flags().StringVar(&traceFile, "trace", "", "where to write trace output")

	return cmd
}

func buildArch(ctx context.Context, cfg *global, arch string, args []string) error {
	archDir := cfg.logdir(arch)
	if err := os.MkdirAll(archDir, os.ModePerm); err != nil {
		return fmt.Errorf("creating buildlogs directory: %w", err)
	}

	newTask := func(pkg string) *task {
		return &task{
			cfg:  cfg,
			pkg:  pkg,
			arch: arch,
			cond: sync.NewCond(&sync.Mutex{}),
			deps: map[string]*task{},
		}
	}

	// We want to ignore info level here during setup, but further down below we pull whatever was passed to use via ctx.
	log := clog.New(charmlog.NewWithOptions(os.Stderr, charmlog.Options{ReportTimestamp: true, Level: charmlog.WarnLevel}))
	setupCtx := clog.WithLogger(ctx, log)

	// Walk all the melange configs in cfg.dir, parses them, and builds the dependency graph of environment + pipelines (build time deps).
	pkgs, err := dag.NewPackages(setupCtx, os.DirFS(cfg.dir), cfg.dir, cfg.pipelineDir)
	if err != nil {
		return err
	}
	g, err := dag.NewGraph(setupCtx, pkgs, dag.WithKeys(cfg.extraKeys...), dag.WithRepos(cfg.extraRepos...), dag.WithArch(arch))
	if err != nil {
		return err
	}

	// This drops any edges to non-local packages. This is a problem for bootstrap because things that depend
	// on bootstrap stages need to be run early.
	g, err = g.Filter(dag.FilterLocal())
	if err != nil {
		return err
	}

	// Only return main packages (configs) because we can't build just subpackages.
	g, err = g.Filter(dag.OnlyMainPackages(pkgs))
	if err != nil {
		return err
	}

	m, err := g.Graph.AdjacencyMap()
	if err != nil {
		return err
	}

	tasks := map[string]*task{}
	for _, pkg := range g.Packages() {
		if tasks[pkg] == nil {
			tasks[pkg] = newTask(pkg)
		}
		for k, v := range m {
			// The package list is in the form of "pkg:version",
			// but we only care about the package name.
			if strings.HasPrefix(k, pkg+":") {
				for _, dep := range v {
					d, _, _ := strings.Cut(dep.Target, ":")

					if tasks[d] == nil {
						tasks[d] = newTask(d)
					}
					tasks[pkg].deps[d] = tasks[d]
				}
			}
		}
	}

	if len(tasks) == 0 {
		return fmt.Errorf("no packages to build")
	}

	todos := tasks
	if len(args) != 0 {
		todos = make(map[string]*task, len(args))

		for _, arg := range args {
			t, ok := tasks[arg]
			if !ok {
				return fmt.Errorf("constraint %q does not exist", arg)
			}
			todos[arg] = t
		}
	}

	for _, t := range todos {
		t.maybeStart(ctx)
	}
	count := len(todos)

	// We're ok with Info level from here on.
	log = clog.FromContext(ctx)

	errs := []error{}
	skipped := 0

	for len(todos) != 0 {
		t := <-cfg.donech
		delete(todos, t.pkg)

		if err := t.err; err != nil {
			errs = append(errs, fmt.Errorf("failed to build %s: %w", t.pkg, err))
			log.Errorf("Failed to build %s (%d/%d)", t.pkg, len(errs), count)
			continue
		}

		// Logging every skipped package is too noisy, so we just print a summary
		// of the number of packages we skipped between actual builds.
		if t.skipped {
			skipped++
			continue
		} else if skipped != 0 {
			log.Infof("Skipped building %d packages", skipped)
			skipped = 0
		}

		log.Infof("Finished building %s (%d/%d)", t.pkg, count-(len(todos)+len(errs)), count)
	}

	// If the context is cancelled, it's not useful to print everything, just summarize the count.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("failed to build %d packages: %w", len(errs), err)
	}

	return errors.Join(errs...)
}

type global struct {
	dryrun bool

	jobch  chan struct{}
	donech chan *task

	dir         string
	pipelineDir string
	runner      string

	archs      []string
	extraKeys  []string
	extraRepos []string

	generateIndex bool

	signingKey  string
	namespace   string
	cacheSource string
	cacheDir    string
	outDir      string

	mu sync.Mutex
}

func (g *global) logdir(arch string) string {
	return filepath.Join(g.outDir, arch, "buildlogs")
}

type task struct {
	cfg *global

	pkg  string
	arch string

	err  error
	deps map[string]*task

	cond    *sync.Cond
	started bool
	done    bool
	skipped bool
}

func (t *task) gitSDE(ctx context.Context, origin string) (string, error) {
	// TODO: Support nested yaml files.
	yamlfile := filepath.Join(t.cfg.dir, origin) + ".yaml"
	cmd := exec.CommandContext(ctx, "git", "log", "-1", "--pretty=%ct", "--follow", yamlfile)
	b, err := cmd.Output()
	if err != nil {
		return "", err
	}

	sde, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return "", err
	}

	return time.Unix(sde, 0).Format(time.RFC3339), nil
}

func (t *task) start(ctx context.Context) {
	defer func() {
		// When we finish, wake up any goroutines that are waiting on us.
		t.cond.L.Lock()
		t.done = true
		t.cond.Broadcast()
		t.cond.L.Unlock()
		t.cfg.donech <- t
	}()

	for _, dep := range t.deps {
		dep.maybeStart(ctx)
	}

	if len(t.deps) != 0 {
		clog.FromContext(ctx).Debugf("task %q waiting on %q", t.pkg, maps.Keys(t.deps))
	}

	for _, dep := range t.deps {
		if err := dep.wait(); err != nil {
			t.err = err
			return
		}
	}

	// Block on jobch, to limit concurrency. Remove from jobch when done.
	t.cfg.jobch <- struct{}{}
	defer func() { <-t.cfg.jobch }()

	// all deps are done and we're clear to launch.
	t.err = t.build(ctx)
}

func (t *task) build(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	log := clog.FromContext(ctx)
	cfg, err := config.ParseConfiguration(ctx, fmt.Sprintf("%s.yaml", t.pkg), config.WithFS(os.DirFS(t.cfg.dir)))
	if err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}

	arch := types.ParseArchitecture(t.arch).ToAPK()

	pkgver := fmt.Sprintf("%s-%s-r%d", cfg.Package.Name, cfg.Package.Version, cfg.Package.Epoch)
	logDir := t.cfg.logdir(arch)
	logfile := filepath.Join(logDir, pkgver) + ".log"

	// See if we already have the package built.
	apk := pkgver + ".apk"
	apkPath := filepath.Join(t.cfg.outDir, arch, apk)
	if _, err := os.Stat(apkPath); err == nil {
		// TODO: Consider the ability to check an APKINDEX instead of requiring gcsfuse to make targets local.
		log.Debugf("Skipping %s, already built", apkPath)
		t.skipped = true
		return nil
	}

	f, err := os.Create(logfile)
	if err != nil {
		return fmt.Errorf("creating logfile: :%w", err)
	}
	defer f.Close()

	log = clog.New(slog.NewTextHandler(f, nil)).With("pkg", t.pkg)
	fctx := clog.WithLogger(ctx, log)

	if len(t.cfg.archs) > 1 {
		log = clog.New(log.Handler()).With("arch", arch)
		fctx = clog.WithLogger(fctx, log)
	}

	sdir := filepath.Join(t.cfg.dir, t.pkg)
	if _, err := os.Stat(sdir); os.IsNotExist(err) {
		if err := os.MkdirAll(sdir, os.ModePerm); err != nil {
			return fmt.Errorf("creating source directory %s: %v", sdir, err)
		}
	} else if err != nil {
		return fmt.Errorf("creating source directory: %v", err)
	}

	fn := fmt.Sprintf("%s.yaml", t.pkg)
	if t.cfg.dryrun {
		log.Infof("DRYRUN: would have built %s", apkPath)
		return nil
	}

	runner, err := newRunner(fctx, t.cfg.runner)
	if err != nil {
		return fmt.Errorf("creating runner: %w", err)
	}

	sde, err := t.gitSDE(ctx, cfg.Package.Name)
	if err != nil {
		return fmt.Errorf("finding source date epoch: %w", err)
	}

	log.Infof("Building %s", t.pkg)
	bc, err := build.New(fctx,
		build.WithArch(types.ParseArchitecture(arch)),
		build.WithConfig(filepath.Join(t.cfg.dir, fn)),
		build.WithPipelineDir(t.cfg.pipelineDir),
		build.WithExtraKeys(t.cfg.extraKeys),
		build.WithExtraRepos(t.cfg.extraRepos),
		build.WithSigningKey(t.cfg.signingKey),
		build.WithRunner(runner),
		build.WithEnvFile(filepath.Join(t.cfg.dir, fmt.Sprintf("build-%s.env", arch))),
		build.WithNamespace(t.cfg.namespace),
		build.WithSourceDir(sdir),
		build.WithCacheSource(t.cfg.cacheSource),
		build.WithCacheDir(t.cfg.cacheDir),
		build.WithOutDir(t.cfg.outDir),
		build.WithBuildDate(sde),
		build.WithRemove(true),
	)
	if errors.Is(err, build.ErrSkipThisArch) {
		log.Warnf("Skipping arch %s", arch)
		t.skipped = true
		return nil
	} else if err != nil {
		return err
	}
	defer func() {
		// We Close() with the original context if we're cancelled so we get cleanup logs to stderr.
		ctx := ctx
		if ctx.Err() == nil {
			// On happy path, we don't care about cleanup logs.
			ctx = fctx
		}

		if err := bc.Close(ctx); err != nil {
			log.Errorf("Closing build %q: %v", t.pkg, err)
		}
	}()

	fctx, span := otel.Tracer("wolfictl").Start(fctx, t.pkg)
	defer span.End()
	if err := bc.BuildPackage(fctx); err != nil {
		return fmt.Errorf("building package (see %q for logs): %w", logfile, err)
	}

	if t.cfg.generateIndex {
		// TODO: We only really need one lock per arch. See if this is a bottleneck.
		t.cfg.mu.Lock()
		defer t.cfg.mu.Unlock()

		packageDir := filepath.Join(t.cfg.outDir, arch)
		log.Infof("Generating apk index from packages in %s", packageDir)

		var apkFiles []string
		apkFiles = append(apkFiles, apkPath)

		for i := range cfg.Subpackages {
			// gocritic complains about copying if you do the normal thing because Subpackages is not a slice of pointers.
			subName := cfg.Subpackages[i].Name

			subpkgApk := fmt.Sprintf("%s-%s-r%d.apk", subName, cfg.Package.Version, cfg.Package.Epoch)
			subpkgFileName := filepath.Join(packageDir, subpkgApk)
			if _, err := os.Stat(subpkgFileName); err != nil {
				log.Warnf("Skipping subpackage %s (was not built): %v", subpkgFileName, err)
				continue
			}
			apkFiles = append(apkFiles, subpkgFileName)
		}

		opts := []index.Option{
			index.WithPackageFiles(apkFiles),
			index.WithSigningKey(t.cfg.signingKey),
			index.WithMergeIndexFileFlag(true),
			index.WithIndexFile(filepath.Join(packageDir, "APKINDEX.tar.gz")),
		}

		idx, err := index.New(opts...)
		if err != nil {
			return fmt.Errorf("unable to create index: %w", err)
		}

		if err := idx.GenerateIndex(fctx); err != nil {
			return fmt.Errorf("unable to generate index: %w", err)
		}
	}

	return nil
}

// If this task hasn't already been started, start it.
func (t *task) maybeStart(ctx context.Context) {
	t.cond.L.Lock()
	defer t.cond.L.Unlock()

	if !t.started {
		t.started = true
		go t.start(ctx)
	}
}

// Park the calling goroutine until this task finishes.
func (t *task) wait() error {
	t.cond.L.Lock()
	for !t.done {
		t.cond.Wait()
	}
	t.cond.L.Unlock()

	return t.err
}

func newRunner(ctx context.Context, runner string) (container.Runner, error) {
	switch runner {
	case "docker":
		return docker.NewRunner(ctx)
	case "bubblewrap":
		return container.BubblewrapRunner(), nil
	}

	return nil, fmt.Errorf("runner %q not supported", runner)
}
