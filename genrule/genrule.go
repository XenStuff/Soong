// Copyright 2015 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package genrule

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/blueprint"
	"github.com/google/blueprint/bootstrap"
	"github.com/google/blueprint/proptools"

	"android/soong/android"
)

func init() {
	registerGenruleBuildComponents(android.InitRegistrationContext)
}

func registerGenruleBuildComponents(ctx android.RegistrationContext) {
	ctx.RegisterModuleType("genrule_defaults", defaultsFactory)

	ctx.RegisterModuleType("gensrcs", GenSrcsFactory)
	ctx.RegisterModuleType("genrule", GenRuleFactory)

	ctx.FinalDepsMutators(func(ctx android.RegisterMutatorsContext) {
		ctx.BottomUp("genrule_tool_deps", toolDepsMutator).Parallel()
	})
}

var (
	pctx = android.NewPackageContext("android/soong/genrule")

	gensrcsMerge = pctx.AndroidStaticRule("gensrcsMerge", blueprint.RuleParams{
		Command:        "${soongZip} -o ${tmpZip} @${tmpZip}.rsp && ${zipSync} -d ${genDir} ${tmpZip}",
		CommandDeps:    []string{"${soongZip}", "${zipSync}"},
		Rspfile:        "${tmpZip}.rsp",
		RspfileContent: "${zipArgs}",
	}, "tmpZip", "genDir", "zipArgs")
)

func init() {
	pctx.Import("android/soong/android")
	pctx.HostBinToolVariable("sboxCmd", "sbox")

	pctx.HostBinToolVariable("soongZip", "soong_zip")
	pctx.HostBinToolVariable("zipSync", "zipsync")
}

type SourceFileGenerator interface {
	GeneratedSourceFiles() android.Paths
	GeneratedHeaderDirs() android.Paths
	GeneratedDeps() android.Paths
}

// Alias for android.HostToolProvider
// Deprecated: use android.HostToolProvider instead.
type HostToolProvider interface {
	android.HostToolProvider
}

type hostToolDependencyTag struct {
	blueprint.BaseDependencyTag
	label string
}

// TODO(cparsons): Move to a common location when there is more than just
// genrule with a bazel_module property.
type bazelModuleProperties struct {
	Label string
}

type generatorProperties struct {
	// The command to run on one or more input files. Cmd supports substitution of a few variables
	//
	// Available variables for substitution:
	//
	//  $(location): the path to the first entry in tools or tool_files
	//  $(location <label>): the path to the tool, tool_file, input or output with name <label>
	//  $(in): one or more input files
	//  $(out): a single output file
	//  $(depfile): a file to which dependencies will be written, if the depfile property is set to true
	//  $(genDir): the sandbox directory for this tool; contains $(out)
	//  $$: a literal $
	Cmd *string

	// Enable reading a file containing dependencies in gcc format after the command completes
	Depfile *bool

	// name of the modules (if any) that produces the host executable.   Leave empty for
	// prebuilts or scripts that do not need a module to build them.
	Tools []string

	// Local file that is used as the tool
	Tool_files []string `android:"path"`

	// List of directories to export generated headers from
	Export_include_dirs []string

	// list of input files
	Srcs []string `android:"path,arch_variant"`

	// input files to exclude
	Exclude_srcs []string `android:"path,arch_variant"`

	// in bazel-enabled mode, the bazel label to evaluate instead of this module
	Bazel_module bazelModuleProperties
}
type Module struct {
	android.ModuleBase
	android.DefaultableModuleBase
	android.ApexModuleBase

	// For other packages to make their own genrules with extra
	// properties
	Extra interface{}
	android.ImageInterface

	properties generatorProperties

	taskGenerator taskFunc

	deps        android.Paths
	rule        blueprint.Rule
	rawCommands []string

	exportedIncludeDirs android.Paths

	outputFiles android.Paths
	outputDeps  android.Paths

	subName string
	subDir  string

	// Collect the module directory for IDE info in java/jdeps.go.
	modulePaths []string
}

type taskFunc func(ctx android.ModuleContext, rawCommand string, srcFiles android.Paths) []generateTask

type generateTask struct {
	in      android.Paths
	out     android.WritablePaths
	depFile android.WritablePath
	copyTo  android.WritablePaths
	genDir  android.WritablePath
	cmd     string
	shard   int
	shards  int
}

func (g *Module) GeneratedSourceFiles() android.Paths {
	return g.outputFiles
}

func (g *Module) Srcs() android.Paths {
	return append(android.Paths{}, g.outputFiles...)
}

func (g *Module) GeneratedHeaderDirs() android.Paths {
	return g.exportedIncludeDirs
}

func (g *Module) GeneratedDeps() android.Paths {
	return g.outputDeps
}

func toolDepsMutator(ctx android.BottomUpMutatorContext) {
	if g, ok := ctx.Module().(*Module); ok {
		for _, tool := range g.properties.Tools {
			tag := hostToolDependencyTag{label: tool}
			if m := android.SrcIsModule(tool); m != "" {
				tool = m
			}
			ctx.AddFarVariationDependencies(ctx.Config().BuildOSTarget.Variations(), tag, tool)
		}
	}
}

// Returns true if information was available from Bazel, false if bazel invocation still needs to occur.
func (c *Module) generateBazelBuildActions(ctx android.ModuleContext, label string) bool {
	bazelCtx := ctx.Config().BazelContext
	filePaths, ok := bazelCtx.GetAllFiles(label)
	if ok {
		var bazelOutputFiles android.Paths
		for _, bazelOutputFile := range filePaths {
			bazelOutputFiles = append(bazelOutputFiles, android.PathForSource(ctx, bazelOutputFile))
		}
		c.outputFiles = bazelOutputFiles
		c.outputDeps = bazelOutputFiles
	}
	return ok
}
func (g *Module) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	g.subName = ctx.ModuleSubDir()

	// Collect the module directory for IDE info in java/jdeps.go.
	g.modulePaths = append(g.modulePaths, ctx.ModuleDir())

	if len(g.properties.Export_include_dirs) > 0 {
		for _, dir := range g.properties.Export_include_dirs {
			g.exportedIncludeDirs = append(g.exportedIncludeDirs,
				android.PathForModuleGen(ctx, g.subDir, ctx.ModuleDir(), dir))
		}
	} else {
		g.exportedIncludeDirs = append(g.exportedIncludeDirs, android.PathForModuleGen(ctx, g.subDir))
	}

	locationLabels := map[string][]string{}
	firstLabel := ""

	addLocationLabel := func(label string, paths []string) {
		if firstLabel == "" {
			firstLabel = label
		}
		if _, exists := locationLabels[label]; !exists {
			locationLabels[label] = paths
		} else {
			ctx.ModuleErrorf("multiple labels for %q, %q and %q",
				label, strings.Join(locationLabels[label], " "), strings.Join(paths, " "))
		}
	}

	if len(g.properties.Tools) > 0 {
		seenTools := make(map[string]bool)

		ctx.VisitDirectDepsBlueprint(func(module blueprint.Module) {
			switch tag := ctx.OtherModuleDependencyTag(module).(type) {
			case hostToolDependencyTag:
				tool := ctx.OtherModuleName(module)
				var path android.OptionalPath

				if t, ok := module.(android.HostToolProvider); ok {
					if !t.(android.Module).Enabled() {
						if ctx.Config().AllowMissingDependencies() {
							ctx.AddMissingDependencies([]string{tool})
						} else {
							ctx.ModuleErrorf("depends on disabled module %q", tool)
						}
						break
					}
					path = t.HostToolPath()
				} else if t, ok := module.(bootstrap.GoBinaryTool); ok {
					if s, err := filepath.Rel(android.PathForOutput(ctx).String(), t.InstallPath()); err == nil {
						path = android.OptionalPathForPath(android.PathForOutput(ctx, s))
					} else {
						ctx.ModuleErrorf("cannot find path for %q: %v", tool, err)
						break
					}
				} else {
					ctx.ModuleErrorf("%q is not a host tool provider", tool)
					break
				}

				if path.Valid() {
					g.deps = append(g.deps, path.Path())
					addLocationLabel(tag.label, []string{path.Path().String()})
					seenTools[tag.label] = true
				} else {
					ctx.ModuleErrorf("host tool %q missing output file", tool)
				}
			}
		})

		// If AllowMissingDependencies is enabled, the build will not have stopped when
		// AddFarVariationDependencies was called on a missing tool, which will result in nonsensical
		// "cmd: unknown location label ..." errors later.  Add a placeholder file to the local label.
		// The command that uses this placeholder file will never be executed because the rule will be
		// replaced with an android.Error rule reporting the missing dependencies.
		if ctx.Config().AllowMissingDependencies() {
			for _, tool := range g.properties.Tools {
				if !seenTools[tool] {
					addLocationLabel(tool, []string{"***missing tool " + tool + "***"})
				}
			}
		}
	}

	if ctx.Failed() {
		return
	}

	for _, toolFile := range g.properties.Tool_files {
		paths := android.PathsForModuleSrc(ctx, []string{toolFile})
		g.deps = append(g.deps, paths...)
		addLocationLabel(toolFile, paths.Strings())
	}

	var srcFiles android.Paths
	for _, in := range g.properties.Srcs {
		paths, missingDeps := android.PathsAndMissingDepsForModuleSrcExcludes(ctx, []string{in}, g.properties.Exclude_srcs)
		if len(missingDeps) > 0 {
			if !ctx.Config().AllowMissingDependencies() {
				panic(fmt.Errorf("should never get here, the missing dependencies %q should have been reported in DepsMutator",
					missingDeps))
			}

			// If AllowMissingDependencies is enabled, the build will not have stopped when
			// the dependency was added on a missing SourceFileProducer module, which will result in nonsensical
			// "cmd: label ":..." has no files" errors later.  Add a placeholder file to the local label.
			// The command that uses this placeholder file will never be executed because the rule will be
			// replaced with an android.Error rule reporting the missing dependencies.
			ctx.AddMissingDependencies(missingDeps)
			addLocationLabel(in, []string{"***missing srcs " + in + "***"})
		} else {
			srcFiles = append(srcFiles, paths...)
			addLocationLabel(in, paths.Strings())
		}
	}

	var copyFrom android.Paths
	var outputFiles android.WritablePaths
	var zipArgs strings.Builder

	for _, task := range g.taskGenerator(ctx, String(g.properties.Cmd), srcFiles) {
		if len(task.out) == 0 {
			ctx.ModuleErrorf("must have at least one output file")
			return
		}

		for _, out := range task.out {
			addLocationLabel(out.Rel(), []string{android.SboxPathForOutput(out, task.genDir)})
		}

		referencedDepfile := false

		rawCommand, err := android.Expand(task.cmd, func(name string) (string, error) {
			// report the error directly without returning an error to android.Expand to catch multiple errors in a
			// single run
			reportError := func(fmt string, args ...interface{}) (string, error) {
				ctx.PropertyErrorf("cmd", fmt, args...)
				return "SOONG_ERROR", nil
			}

			switch name {
			case "location":
				if len(g.properties.Tools) == 0 && len(g.properties.Tool_files) == 0 {
					return reportError("at least one `tools` or `tool_files` is required if $(location) is used")
				}
				paths := locationLabels[firstLabel]
				if len(paths) == 0 {
					return reportError("default label %q has no files", firstLabel)
				} else if len(paths) > 1 {
					return reportError("default label %q has multiple files, use $(locations %s) to reference it",
						firstLabel, firstLabel)
				}
				return locationLabels[firstLabel][0], nil
			case "in":
				return strings.Join(srcFiles.Strings(), " "), nil
			case "out":
				var sandboxOuts []string
				for _, out := range task.out {
					sandboxOuts = append(sandboxOuts, android.SboxPathForOutput(out, task.genDir))
				}
				return strings.Join(sandboxOuts, " "), nil
			case "depfile":
				referencedDepfile = true
				if !Bool(g.properties.Depfile) {
					return reportError("$(depfile) used without depfile property")
				}
				return "__SBOX_DEPFILE__", nil
			case "genDir":
				return android.SboxPathForOutput(task.genDir, task.genDir), nil
			default:
				if strings.HasPrefix(name, "location ") {
					label := strings.TrimSpace(strings.TrimPrefix(name, "location "))
					if paths, ok := locationLabels[label]; ok {
						if len(paths) == 0 {
							return reportError("label %q has no files", label)
						} else if len(paths) > 1 {
							return reportError("label %q has multiple files, use $(locations %s) to reference it",
								label, label)
						}
						return paths[0], nil
					} else {
						return reportError("unknown location label %q", label)
					}
				} else if strings.HasPrefix(name, "locations ") {
					label := strings.TrimSpace(strings.TrimPrefix(name, "locations "))
					if paths, ok := locationLabels[label]; ok {
						if len(paths) == 0 {
							return reportError("label %q has no files", label)
						}
						return strings.Join(paths, " "), nil
					} else {
						return reportError("unknown locations label %q", label)
					}
				} else {
					return reportError("unknown variable '$(%s)'", name)
				}
			}
		})

		if err != nil {
			ctx.PropertyErrorf("cmd", "%s", err.Error())
			return
		}

		if Bool(g.properties.Depfile) && !referencedDepfile {
			ctx.PropertyErrorf("cmd", "specified depfile=true but did not include a reference to '${depfile}' in cmd")
			return
		}
		g.rawCommands = append(g.rawCommands, rawCommand)

		// Pick a unique rule name and the user-visible description.
		desc := "generate"
		name := "generator"
		if task.shards > 0 {
			desc += " " + strconv.Itoa(task.shard)
			name += strconv.Itoa(task.shard)
		} else if len(task.out) == 1 {
			desc += " " + task.out[0].Base()
		}

		// Use a RuleBuilder to create a rule that runs the command inside an sbox sandbox.
		rule := android.NewRuleBuilder().Sbox(task.genDir)
		cmd := rule.Command()
		cmd.Text(rawCommand)
		cmd.ImplicitOutputs(task.out)
		cmd.Implicits(task.in)
		cmd.Implicits(g.deps)
		if Bool(g.properties.Depfile) {
			cmd.ImplicitDepFile(task.depFile)
		}

		// Create the rule to run the genrule command inside sbox.
		rule.Build(pctx, ctx, name, desc)

		if len(task.copyTo) > 0 {
			// If copyTo is set, multiple shards need to be copied into a single directory.
			// task.out contains the per-shard paths, and copyTo contains the corresponding
			// final path.  The files need to be copied into the final directory by a
			// single rule so it can remove the directory before it starts to ensure no
			// old files remain.  zipsync already does this, so build up zipArgs that
			// zip all the per-shard directories into a single zip.
			outputFiles = append(outputFiles, task.copyTo...)
			copyFrom = append(copyFrom, task.out.Paths()...)
			zipArgs.WriteString(" -C " + task.genDir.String())
			zipArgs.WriteString(android.JoinWithPrefix(task.out.Strings(), " -f "))
		} else {
			outputFiles = append(outputFiles, task.out...)
		}
	}

	if len(copyFrom) > 0 {
		// Create a rule that zips all the per-shard directories into a single zip and then
		// uses zipsync to unzip it into the final directory.
		ctx.Build(pctx, android.BuildParams{
			Rule:      gensrcsMerge,
			Implicits: copyFrom,
			Outputs:   outputFiles,
			Args: map[string]string{
				"zipArgs": zipArgs.String(),
				"tmpZip":  android.PathForModuleGen(ctx, g.subDir+".zip").String(),
				"genDir":  android.PathForModuleGen(ctx, g.subDir).String(),
			},
		})
	}

	g.outputFiles = outputFiles.Paths()

	bazelModuleLabel := g.properties.Bazel_module.Label
	bazelActionsUsed := false
	if ctx.Config().BazelContext.BazelEnabled() && len(bazelModuleLabel) > 0 {
		bazelActionsUsed = g.generateBazelBuildActions(ctx, bazelModuleLabel)
	}
	if !bazelActionsUsed {
		// For <= 6 outputs, just embed those directly in the users. Right now, that covers >90% of
		// the genrules on AOSP. That will make things simpler to look at the graph in the common
		// case. For larger sets of outputs, inject a phony target in between to limit ninja file
		// growth.
		if len(g.outputFiles) <= 6 {
			g.outputDeps = g.outputFiles
		} else {
			phonyFile := android.PathForModuleGen(ctx, "genrule-phony")
			ctx.Build(pctx, android.BuildParams{
				Rule:   blueprint.Phony,
				Output: phonyFile,
				Inputs: g.outputFiles,
			})
			g.outputDeps = android.Paths{phonyFile}
		}
	}
}

// Collect information for opening IDE project files in java/jdeps.go.
func (g *Module) IDEInfo(dpInfo *android.IdeInfo) {
	dpInfo.Srcs = append(dpInfo.Srcs, g.Srcs().Strings()...)
	for _, src := range g.properties.Srcs {
		if strings.HasPrefix(src, ":") {
			src = strings.Trim(src, ":")
			dpInfo.Deps = append(dpInfo.Deps, src)
		}
	}
	dpInfo.Paths = append(dpInfo.Paths, g.modulePaths...)
}

func (g *Module) AndroidMk() android.AndroidMkData {
	return android.AndroidMkData{
		Class:      "ETC",
		OutputFile: android.OptionalPathForPath(g.outputFiles[0]),
		SubName:    g.subName,
		Extra: []android.AndroidMkExtraFunc{
			func(w io.Writer, outputFile android.Path) {
				fmt.Fprintln(w, "LOCAL_UNINSTALLABLE_MODULE := true")
			},
		},
		Custom: func(w io.Writer, name, prefix, moduleDir string, data android.AndroidMkData) {
			android.WriteAndroidMkData(w, data)
			if data.SubName != "" {
				fmt.Fprintln(w, ".PHONY:", name)
				fmt.Fprintln(w, name, ":", name+g.subName)
			}
		},
	}
}

func (g *Module) ShouldSupportSdkVersion(ctx android.BaseModuleContext,
	sdkVersion android.ApiLevel) error {
	// Because generated outputs are checked by client modules(e.g. cc_library, ...)
	// we can safely ignore the check here.
	return nil
}

func generatorFactory(taskGenerator taskFunc, props ...interface{}) *Module {
	module := &Module{
		taskGenerator: taskGenerator,
	}

	module.AddProperties(props...)
	module.AddProperties(&module.properties)

	module.ImageInterface = noopImageInterface{}

	return module
}

type noopImageInterface struct{}

func (x noopImageInterface) ImageMutatorBegin(android.BaseModuleContext)                 {}
func (x noopImageInterface) CoreVariantNeeded(android.BaseModuleContext) bool            { return false }
func (x noopImageInterface) RamdiskVariantNeeded(android.BaseModuleContext) bool         { return false }
func (x noopImageInterface) VendorRamdiskVariantNeeded(android.BaseModuleContext) bool   { return false }
func (x noopImageInterface) RecoveryVariantNeeded(android.BaseModuleContext) bool        { return false }
func (x noopImageInterface) ExtraImageVariations(ctx android.BaseModuleContext) []string { return nil }
func (x noopImageInterface) SetImageVariation(ctx android.BaseModuleContext, variation string, module android.Module) {
}

func NewGenSrcs() *Module {
	properties := &genSrcsProperties{}

	taskGenerator := func(ctx android.ModuleContext, rawCommand string, srcFiles android.Paths) []generateTask {
		genDir := android.PathForModuleGen(ctx, "gensrcs")
		shardSize := defaultShardSize
		if s := properties.Shard_size; s != nil {
			shardSize = int(*s)
		}

		shards := android.ShardPaths(srcFiles, shardSize)
		var generateTasks []generateTask

		for i, shard := range shards {
			var commands []string
			var outFiles android.WritablePaths
			var copyTo android.WritablePaths
			var shardDir android.WritablePath
			var depFile android.WritablePath

			if len(shards) > 1 {
				shardDir = android.PathForModuleGen(ctx, strconv.Itoa(i))
			} else {
				shardDir = genDir
			}

			for j, in := range shard {
				outFile := android.GenPathWithExt(ctx, "gensrcs", in, String(properties.Output_extension))
				if j == 0 {
					depFile = outFile.ReplaceExtension(ctx, "d")
				}

				if len(shards) > 1 {
					shardFile := android.GenPathWithExt(ctx, strconv.Itoa(i), in, String(properties.Output_extension))
					copyTo = append(copyTo, outFile)
					outFile = shardFile
				}

				outFiles = append(outFiles, outFile)

				command, err := android.Expand(rawCommand, func(name string) (string, error) {
					switch name {
					case "in":
						return in.String(), nil
					case "out":
						return android.SboxPathForOutput(outFile, shardDir), nil
					default:
						return "$(" + name + ")", nil
					}
				})
				if err != nil {
					ctx.PropertyErrorf("cmd", err.Error())
				}

				// escape the command in case for example it contains '#', an odd number of '"', etc
				command = fmt.Sprintf("bash -c %v", proptools.ShellEscape(command))
				commands = append(commands, command)
			}
			fullCommand := strings.Join(commands, " && ")

			generateTasks = append(generateTasks, generateTask{
				in:      shard,
				out:     outFiles,
				depFile: depFile,
				copyTo:  copyTo,
				genDir:  shardDir,
				cmd:     fullCommand,
				shard:   i,
				shards:  len(shards),
			})
		}

		return generateTasks
	}

	g := generatorFactory(taskGenerator, properties)
	g.subDir = "gensrcs"
	return g
}

func GenSrcsFactory() android.Module {
	m := NewGenSrcs()
	android.InitAndroidModule(m)
	return m
}

type genSrcsProperties struct {
	// extension that will be substituted for each output file
	Output_extension *string

	// maximum number of files that will be passed on a single command line.
	Shard_size *int64
}

const defaultShardSize = 100

func NewGenRule() *Module {
	properties := &genRuleProperties{}

	taskGenerator := func(ctx android.ModuleContext, rawCommand string, srcFiles android.Paths) []generateTask {
		outs := make(android.WritablePaths, len(properties.Out))
		var depFile android.WritablePath
		for i, out := range properties.Out {
			outPath := android.PathForModuleGen(ctx, out)
			if i == 0 {
				depFile = outPath.ReplaceExtension(ctx, "d")
			}
			outs[i] = outPath
		}
		return []generateTask{{
			in:      srcFiles,
			out:     outs,
			depFile: depFile,
			genDir:  android.PathForModuleGen(ctx),
			cmd:     rawCommand,
		}}
	}

	return generatorFactory(taskGenerator, properties)
}

func GenRuleFactory() android.Module {
	m := NewGenRule()
	android.InitAndroidModule(m)
	android.InitDefaultableModule(m)
	return m
}

type genRuleProperties struct {
	// names of the output files that will be generated
	Out []string `android:"arch_variant"`
}

var Bool = proptools.Bool
var String = proptools.String

//
// Defaults
//
type Defaults struct {
	android.ModuleBase
	android.DefaultsModuleBase
}

func defaultsFactory() android.Module {
	return DefaultsFactory()
}

func DefaultsFactory(props ...interface{}) android.Module {
	module := &Defaults{}

	module.AddProperties(props...)
	module.AddProperties(
		&generatorProperties{},
		&genRuleProperties{},
	)

	android.InitDefaultsModule(module)

	return module
}