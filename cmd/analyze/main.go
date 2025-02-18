package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"

	"github.com/ossf/package-analysis/internal/analysis"
	"github.com/ossf/package-analysis/internal/featureflags"
	"github.com/ossf/package-analysis/internal/log"
	"github.com/ossf/package-analysis/internal/pkgmanager"
	"github.com/ossf/package-analysis/internal/resultstore"
	"github.com/ossf/package-analysis/internal/sandbox"
	"github.com/ossf/package-analysis/internal/staticanalysis"
	"github.com/ossf/package-analysis/internal/utils"
	"github.com/ossf/package-analysis/internal/worker"
	"github.com/ossf/package-analysis/pkg/api/pkgecosystem"
)

var (
	pkgName             = flag.String("package", "", "package name")
	localPkg            = flag.String("local", "", "local package path")
	ecosystem           pkgecosystem.Ecosystem
	version             = flag.String("version", "", "version")
	noPull              = flag.Bool("nopull", false, "disables pulling down sandbox images")
	imageTag            = flag.String("image-tag", "", "set image tag for analysis sandboxes")
	dynamicUpload       = flag.String("upload", "", "bucket path for uploading dynamic analysis results")
	staticUpload        = flag.String("upload-static", "", "bucket path for uploading static analysis results")
	uploadFileWriteInfo = flag.String("upload-file-write-info", "", "bucket path for uploading information from file writes")
	uploadAnalyzedPkg   = flag.String("upload-analyzed-pkg", "", "bucket path for uploading analyzed packages")
	offline             = flag.Bool("offline", false, "disables sandbox network access")
	customSandbox       = flag.String("sandbox-image", "", "override default dynamic analysis sandbox with custom image")
	customAnalysisCmd   = flag.String("analysis-command", "", "override default dynamic analysis script path (use with custom sandbox image)")
	listModes           = flag.Bool("list-modes", false, "prints out a list of available analysis modes")
	features            = flag.String("features", "", "override features that are enabled/disabled by default")
	listFeatures        = flag.Bool("list-features", false, "list available features that can be toggled")
	help                = flag.Bool("help", false, "print help on available options")
	analysisMode        = utils.CommaSeparatedFlags("mode", []string{"static", "dynamic"},
		"list of analysis modes to run, separated by commas. Use -list-modes to see available options")
)

func makeResultStores() worker.ResultStores {
	rs := worker.ResultStores{}

	if *dynamicUpload != "" {
		rs.DynamicAnalysis = resultstore.New(*dynamicUpload)
	}
	if *staticUpload != "" {
		rs.StaticAnalysis = resultstore.New(*staticUpload)
	}
	if *uploadFileWriteInfo != "" {
		rs.FileWrites = resultstore.New(*uploadFileWriteInfo)
	}

	if *uploadAnalyzedPkg != "" {
		rs.AnalyzedPackage = resultstore.New(*uploadAnalyzedPkg)
	}

	return rs
}

func printAnalysisModes() {
	fmt.Println("Available analysis modes:")
	for _, mode := range analysis.AllModes() {
		fmt.Println(mode)
	}
	fmt.Println()
}

func printFeatureFlags() {
	fmt.Printf("Feature List\n\n")
	fmt.Printf("%-30s %s\n", "Name", "Default")
	fmt.Printf("----------------------------------------\n")

	// print features in sorted order
	state := featureflags.State()
	sortedFeatures := maps.Keys(state)
	slices.Sort(sortedFeatures)

	// print Off/On rather than 'false' and 'true'
	stateStrings := map[bool]string{false: "Off", true: "On"}
	for _, feature := range sortedFeatures {
		fmt.Printf("%-30s %s\n", feature, stateStrings[state[feature]])
	}

	fmt.Println()
}

// makeSandboxOptions prepares options for the sandbox based on command line arguments.
//
// In particular:
//
//  1. The image tag is always passed through. An empty tag is the same as "latest".
//  2. A local package is mapped into the sandbox if applicable.
//  3. Image pulling is disabled if the "-nopull" command-line flag was used.
func makeSandboxOptions() []sandbox.Option {
	sbOpts := []sandbox.Option{sandbox.Tag(*imageTag)}

	if *localPkg != "" {
		sbOpts = append(sbOpts, sandbox.Copy(*localPkg, *localPkg))
	}
	if *noPull {
		sbOpts = append(sbOpts, sandbox.NoPull())
	}
	if *offline {
		sbOpts = append(sbOpts, sandbox.Offline())
	}

	return sbOpts
}

func dynamicAnalysis(pkg *pkgmanager.Pkg, resultStores *worker.ResultStores) {
	if !*offline {
		sandbox.InitNetwork()
	}

	sbOpts := append(worker.DynamicSandboxOptions(), makeSandboxOptions()...)

	if *customSandbox != "" {
		sbOpts = append(sbOpts, sandbox.Image(*customSandbox))
	}

	result, err := worker.RunDynamicAnalysis(pkg, sbOpts, *customAnalysisCmd)
	if err != nil {
		log.Error("Dynamic analysis aborted (run error)", "error", err)
		return
	}

	// this is only valid if RunDynamicAnalysis() returns nil err
	if result.LastStatus != analysis.StatusCompleted {
		log.Warn("Dynamic analysis phase did not complete successfully",
			"lastRunPhase", result.LastRunPhase,
			"status", result.LastStatus)
	}

	ctx := context.Background()
	if err := worker.SaveDynamicAnalysisData(ctx, pkg, resultStores, result.AnalysisData); err != nil {
		log.Error("Upload error", "error", err)
	}
}

func staticAnalysis(pkg *pkgmanager.Pkg, resultStores *worker.ResultStores) {
	if !*offline {
		sandbox.InitNetwork()
	}

	sbOpts := append(worker.StaticSandboxOptions(), makeSandboxOptions()...)

	data, status, err := worker.RunStaticAnalysis(pkg, sbOpts, staticanalysis.All)
	if err != nil {
		log.Error("Static analysis aborted", "error", err)
		return
	}

	log.Info("Static analysis completed", "status", status)

	ctx := context.Background()
	if err := worker.SaveStaticAnalysisData(ctx, pkg, resultStores, data); err != nil {
		log.Error("Upload error", "error", err)
	}
}

func main() {
	log.Initialize(os.Getenv("LOGGER_ENV"))

	flag.TextVar(&ecosystem, "ecosystem", pkgecosystem.None, fmt.Sprintf("package ecosystem. Can be %s", pkgecosystem.SupportedEcosystemsStrings))

	analysisMode.InitFlag()
	flag.Parse()

	if err := featureflags.Update(*features); err != nil {
		log.Fatal("Failed to parse flags", "error", err)
		return
	}

	if *help {
		flag.Usage()
		return
	}

	if *listModes {
		printAnalysisModes()
		return
	}

	if *listFeatures {
		printFeatureFlags()
		return
	}

	if ecosystem == pkgecosystem.None {
		flag.Usage()
		return
	}

	manager := pkgmanager.Manager(ecosystem)
	if manager == nil {
		log.Panic("Unsupported pkg manager",
			log.Label("ecosystem", string(ecosystem)))
	}

	if *pkgName == "" {
		flag.Usage()
		return
	}

	runMode := make(map[analysis.Mode]bool)
	for _, analysisName := range analysisMode.Values {
		mode, ok := analysis.ModeFromString(strings.ToLower(analysisName))
		if !ok {
			log.Error("Unknown analysis mode: " + analysisName)
			printAnalysisModes()
			return
		}
		runMode[mode] = true
	}

	worker.LogRequest(ecosystem, *pkgName, *version, *localPkg, "")

	pkg, err := worker.ResolvePkg(manager, *pkgName, *version, *localPkg)
	if err != nil {
		log.Panic("Error resolving package",
			log.Label("ecosystem", ecosystem.String()),
			"name", *pkgName,
			"error", err)
	}

	resultStores := makeResultStores()

	if runMode[analysis.Static] {
		log.Info("Starting static analysis")
		staticAnalysis(pkg, &resultStores)
	}

	// dynamicAnalysis() currently panics on error, so it's last
	if runMode[analysis.Dynamic] {
		log.Info("Starting dynamic analysis")
		dynamicAnalysis(pkg, &resultStores)
	}
}
