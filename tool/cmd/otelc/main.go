// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// 阅读入口：otelc 把“准备插桩依赖”和“接管 Go 编译命令”放在同一个可执行文件里。
// 用户运行 otelc go build 时，本进程先执行 setup，再启动带 -toolexec 的 go build；
// Go 工具链随后会为编译、汇编、链接等任务再次启动 otelc toolexec 子进程。
// 因而这里的命令注册、环境变量和 context 不只服务于一次普通 CLI 调用。
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/urfave/cli/v3"

	"go.opentelemetry.io/otelc/tool/ex"
	"go.opentelemetry.io/otelc/tool/util"
)

const (
	debugLogFilename = "debug.log"
	flagWorkDir      = "work-dir"
)

func main() {
	// cli.Command 是命令树；Action 负责业务，Before/After 承担共享资源的初始化与释放。
	// 真正的两阶段构建入口在 command.go → setup.GoBuild，而不是直接在 main 中插桩。
	app := cli.Command{
		Name:        "otelc",
		Usage:       "OpenTelemetry Go Compile-Time Instrumentation Tool",
		HideVersion: true,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:      flagWorkDir,
				Aliases:   []string{"w"},
				Usage:     "The path to a directory where working files will be written",
				TakesFile: true,
				Value:     util.GetOtelcWorkDir(),
			},
			&cli.BoolFlag{
				Name:    "debug",
				Aliases: []string{"d"},
				Sources: cli.EnvVars(util.EnvOtelcDebug),
				Usage:   "Enable debug mode",
				Value:   false,
			},
			&cli.StringFlag{
				Name:      "rules",
				Aliases:   []string{"rules"},
				Usage:     "The path to the rules configuration file",
				TakesFile: true,
				Value:     "",
			},
			&cli.StringFlag{
				Name:    "profile-path",
				Sources: cli.EnvVars(envProfilePath),
				Usage:   "Directory for profiling output",
				Hidden:  true,
			},
			&cli.StringSliceFlag{
				Name:    "profile",
				Sources: cli.EnvVars(envEnabledProfiles),
				Usage:   "Enable profiling: cpu, heap, trace (repeatable)",
				Hidden:  true,
			},
			&cli.BoolFlag{
				Name:    "profile-summary",
				Sources: cli.EnvVars("OTELC_PROFILE_SUMMARY"),
				Usage:   "Merge profile files into one per type after build completes",
				Hidden:  true,
			},
			&cli.BoolFlag{
				Name:    "stats",
				Sources: cli.EnvVars(util.EnvOtelcStats),
				Usage:   "Log per-tool wall-clock duration for toolexec commands",
				Hidden:  true,
			},
		},
		Commands: []*cli.Command{
			&commandPin,
			&commandSetup,
			&commandGo,
			&commandCleanup,
			&commandToolexec,
			&commandVersion,
		},
		Before: func(ctx context.Context, cmd *cli.Command) (context.Context, error) {
			// GOFLAGS 会被新启动的 go 命令继承。若把外层的 -toolexec 原样传下去，
			// setup 内部用于分析依赖的 go 命令也可能重新进入 otelc，形成递归调用。
			// 先清除继承的包装器，再由需要插桩的构建路径显式重新添加。
			// In drop-in mode (otelc is the GOFLAGS -toolexec tool) the go
			// children otelc spawns would inherit the flag and recurse, so
			// strip it process-wide; commands that need toolexec re-add it.
			if goflags := os.Getenv("GOFLAGS"); goflags != "" {
				if stripped := util.StripToolexecFromGoflags(goflags); stripped != goflags {
					if err := os.Setenv("GOFLAGS", stripped); err != nil {
						return ctx, ex.Wrapf(err, "stripping -toolexec from GOFLAGS")
					}
				}
			}
			ctx, err := initLogger(ctx, cmd)
			if err != nil {
				return ctx, err
			}
			ctx, err = initProfiling(ctx, cmd)
			if err != nil {
				return ctx, err
			}
			return initStats(ctx, cmd)
		},
		After: func(ctx context.Context, cmd *cli.Command) error {
			return ex.Join(stopProfiling(ctx, cmd), closeLogger(ctx))
		},
	}

	// context.Context 沿调用链传递取消信号；耗时的包加载、子进程和锁等待可响应退出。
	// defer 按函数返回时执行，确保正常返回路径也释放信号通知相关资源。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := app.Run(ctx, os.Args)
	if err != nil {
		ex.Fatal(err)
	}
}

func initLogger(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	// 工作目录采用绝对路径，并通过环境变量告诉所有 toolexec 子进程；
	// 这些子进程的当前目录可能是依赖源码目录，不能据此定位同一份 matched.json。
	workDir, err := filepath.Abs(cmd.String(flagWorkDir))
	if err != nil {
		return ctx, ex.Wrapf(err, "failed to resolve work directory %q", cmd.String(flagWorkDir))
	}

	// Bare -toolexec (GOFLAGS drop-in): no parent set OTELC_WORK_DIR, and the
	// toolchain may run us from the package source dir (asm runs in the
	// read-only module cache). Discover the dir from `otelc setup` rather than
	// trusting cwd; skip filesystem setup when none is found instead of
	// creating .otelc-build in an arbitrary location.
	if cmd.Args().First() == "toolexec" && !cmd.IsSet(flagWorkDir) && os.Getenv(util.EnvOtelcWorkDir) == "" {
		workDir = util.DiscoverWorkDir(workDir)
		if workDir == "" {
			return ctx, nil
		}
	}

	if setErr := os.Setenv(util.EnvOtelcWorkDir, workDir); setErr != nil {
		return ctx, ex.Wrapf(setErr, "failed to set %s", util.EnvOtelcWorkDir)
	}

	// Skip filesystem setup for subcommands that don't produce artifacts or
	// that remove .otelc-build/ (opening a file there would prevent deletion on Windows).
	switch cmd.Args().First() {
	case "version", "cleanup":
		return ctx, nil
	}

	buildTempDir := util.GetBuildTempDir()
	err = os.MkdirAll(buildTempDir, 0o755)
	if err != nil {
		return ctx, ex.Wrapf(err, "failed to create work directory %q", buildTempDir)
	}

	logFilename := filepath.Join(buildTempDir, debugLogFilename)
	logFile, err := os.OpenFile(logFilename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return ctx, ex.Wrapf(err, "failed to open log file %q", logFilename)
	}

	level := slog.LevelInfo
	if cmd.Bool("debug") {
		level = slog.LevelDebug
		if setErr := os.Setenv(util.EnvOtelcDebug, "1"); setErr != nil {
			_ = logFile.Close()
			return ctx, ex.Wrapf(setErr, "set %s", util.EnvOtelcDebug)
		}
	}

	// Log timestamps and levels are omitted: they add noise when correlating
	// with Go toolchain output and the log file is for human debugging only.
	handler := slog.NewTextHandler(logFile, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey || a.Key == slog.LevelKey {
				return slog.Attr{}
			}
			return a
		},
		Level: level,
	})
	logger := slog.New(handler)
	// WithValue 风格的辅助函数返回一个派生 context，不会原地修改旧 context。
	// 必须接住返回值，后续阶段才能取到同一个 logger 与需要关闭的日志文件。
	ctx = util.ContextWithLogger(ctx, logger)
	// closeLogger is safe to call from the After hook: cli/v3 After is a defer
	// closure that captures the ctx variable by reference, so the updated ctx
	// from Before is visible when After runs.
	ctx = util.ContextWithLogWriter(ctx, logFile)

	return ctx, nil
}

func closeLogger(ctx context.Context) error {
	writer := util.LogWriterFromContext(ctx)
	if writer == nil {
		return nil
	}
	return writer.Close()
}

func addLoggerPhaseAttribute(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	logger := util.LoggerFromContext(ctx)
	logger = logger.With("phase", cmd.Name)
	return util.ContextWithLogger(ctx, logger), nil
}
