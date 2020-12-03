// Copyright (C) 2019 The Android Open Source Project
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

package apex

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"android/soong/android"
	"android/soong/cc"
	"android/soong/java"

	"github.com/google/blueprint/proptools"
)

func (a *apexBundle) AndroidMk() android.AndroidMkData {
	if a.properties.HideFromMake {
		return android.AndroidMkData{
			Disabled: true,
		}
	}
	return a.androidMkForType()
}

// nameInMake converts apexFileClass into the corresponding class name in Make.
func (class apexFileClass) nameInMake() string {
	switch class {
	case etc:
		return "ETC"
	case nativeSharedLib:
		return "SHARED_LIBRARIES"
	case nativeExecutable, shBinary, pyBinary, goBinary:
		return "EXECUTABLES"
	case javaSharedLib:
		return "JAVA_LIBRARIES"
	case nativeTest:
		return "NATIVE_TESTS"
	case app, appSet:
		// b/142537672 Why isn't this APP? We want to have full control over
		// the paths and file names of the apk file under the flattend APEX.
		// If this is set to APP, then the paths and file names are modified
		// by the Make build system. For example, it is installed to
		// /system/apex/<apexname>/app/<Appname>/<apexname>.<Appname>/ instead of
		// /system/apex/<apexname>/app/<Appname> because the build system automatically
		// appends module name (which is <apexname>.<Appname> to the path.
		return "ETC"
	default:
		panic(fmt.Errorf("unknown class %d", class))
	}
}

func (a *apexBundle) androidMkForFiles(w io.Writer, apexBundleName, apexName, moduleDir string,
	apexAndroidMkData android.AndroidMkData) []string {

	// apexBundleName comes from the 'name' property; apexName comes from 'apex_name' property.
	// An apex is installed to /system/apex/<apexBundleName> and is activated at /apex/<apexName>
	// In many cases, the two names are the same, but could be different in general.

	moduleNames := []string{}
	apexType := a.properties.ApexType
	// To avoid creating duplicate build rules, run this function only when primaryApexType is true
	// to install symbol files in $(PRODUCT_OUT}/apex.
	// And if apexType is flattened, run this function to install files in $(PRODUCT_OUT}/system/apex.
	if !a.primaryApexType && apexType != flattenedApex {
		return moduleNames
	}

	// b/162366062. Prevent GKI APEXes to emit make rules to avoid conflicts.
	if strings.HasPrefix(apexName, "com.android.gki.") && apexType != flattenedApex {
		return moduleNames
	}

	// b/140136207. When there are overriding APEXes for a VNDK APEX, the symbols file for the overridden
	// APEX and the overriding APEX will have the same installation paths at /apex/com.android.vndk.v<ver>
	// as their apexName will be the same. To avoid the path conflicts, skip installing the symbol files
	// for the overriding VNDK APEXes.
	symbolFilesNotNeeded := a.vndkApex && len(a.overridableProperties.Overrides) > 0
	if symbolFilesNotNeeded && apexType != flattenedApex {
		return moduleNames
	}

	var postInstallCommands []string
	for _, fi := range a.filesInfo {
		if a.linkToSystemLib && fi.transitiveDep && fi.availableToPlatform() {
			// TODO(jiyong): pathOnDevice should come from fi.module, not being calculated here
			linkTarget := filepath.Join("/system", fi.path())
			linkPath := filepath.Join(a.installDir.ToMakePath().String(), apexBundleName, fi.path())
			mkdirCmd := "mkdir -p " + filepath.Dir(linkPath)
			linkCmd := "ln -sfn " + linkTarget + " " + linkPath
			postInstallCommands = append(postInstallCommands, mkdirCmd, linkCmd)
		}
	}

	seenDataOutPaths := make(map[string]bool)

	for _, fi := range a.filesInfo {
		if ccMod, ok := fi.module.(*cc.Module); ok && ccMod.Properties.HideFromMake {
			continue
		}

		linkToSystemLib := a.linkToSystemLib && fi.transitiveDep && fi.availableToPlatform()

		var moduleName string
		if linkToSystemLib {
			moduleName = fi.androidMkModuleName
		} else {
			moduleName = fi.androidMkModuleName + "." + apexBundleName + a.suffix
		}

		if !android.InList(moduleName, moduleNames) {
			moduleNames = append(moduleNames, moduleName)
		}

		if linkToSystemLib {
			// No need to copy the file since it's linked to the system file
			continue
		}

		fmt.Fprintln(w, "\ninclude $(CLEAR_VARS)")
		if fi.moduleDir != "" {
			fmt.Fprintln(w, "LOCAL_PATH :=", fi.moduleDir)
		} else {
			fmt.Fprintln(w, "LOCAL_PATH :=", moduleDir)
		}
		fmt.Fprintln(w, "LOCAL_MODULE :=", moduleName)
		if fi.module != nil && fi.module.Owner() != "" {
			fmt.Fprintln(w, "LOCAL_MODULE_OWNER :=", fi.module.Owner())
		}
		// /apex/<apex_name>/{lib|framework|...}
		pathWhenActivated := filepath.Join("$(PRODUCT_OUT)", "apex", apexName, fi.installDir)
		var modulePath string
		if apexType == flattenedApex {
			// /system/apex/<name>/{lib|framework|...}
			modulePath = filepath.Join(a.installDir.ToMakePath().String(), apexBundleName, fi.installDir)
			fmt.Fprintln(w, "LOCAL_MODULE_PATH :=", modulePath)
			if a.primaryApexType && !symbolFilesNotNeeded {
				fmt.Fprintln(w, "LOCAL_SOONG_SYMBOL_PATH :=", pathWhenActivated)
			}
			if len(fi.symlinks) > 0 {
				fmt.Fprintln(w, "LOCAL_MODULE_SYMLINKS :=", strings.Join(fi.symlinks, " "))
			}
			newDataPaths := []android.DataPath{}
			for _, path := range fi.dataPaths {
				dataOutPath := modulePath + ":" + path.SrcPath.Rel()
				if ok := seenDataOutPaths[dataOutPath]; !ok {
					newDataPaths = append(newDataPaths, path)
					seenDataOutPaths[dataOutPath] = true
				}
			}
			if len(newDataPaths) > 0 {
				fmt.Fprintln(w, "LOCAL_TEST_DATA :=", strings.Join(android.AndroidMkDataPaths(newDataPaths), " "))
			}

			if fi.module != nil && len(fi.module.NoticeFiles()) > 0 {
				fmt.Fprintln(w, "LOCAL_NOTICE_FILE :=", strings.Join(fi.module.NoticeFiles().Strings(), " "))
			}
		} else {
			modulePath = pathWhenActivated
			fmt.Fprintln(w, "LOCAL_MODULE_PATH :=", pathWhenActivated)

			// For non-flattend APEXes, the merged notice file is attached to the APEX itself.
			// We don't need to have notice file for the individual modules in it. Otherwise,
			// we will have duplicated notice entries.
			fmt.Fprintln(w, "LOCAL_NO_NOTICE_FILE := true")
		}
		fmt.Fprintln(w, "LOCAL_PREBUILT_MODULE_FILE :=", fi.builtFile.String())
		fmt.Fprintln(w, "LOCAL_MODULE_CLASS :=", fi.class.nameInMake())
		if fi.module != nil {
			archStr := fi.module.Target().Arch.ArchType.String()
			host := false
			switch fi.module.Target().Os.Class {
			case android.Host:
				if fi.module.Target().HostCross {
					if fi.module.Target().Arch.ArchType != android.Common {
						fmt.Fprintln(w, "LOCAL_MODULE_HOST_CROSS_ARCH :=", archStr)
					}
				} else {
					if fi.module.Target().Arch.ArchType != android.Common {
						fmt.Fprintln(w, "LOCAL_MODULE_HOST_ARCH :=", archStr)
					}
				}
				host = true
			case android.Device:
				if fi.module.Target().Arch.ArchType != android.Common {
					fmt.Fprintln(w, "LOCAL_MODULE_TARGET_ARCH :=", archStr)
				}
			}
			if host {
				makeOs := fi.module.Target().Os.String()
				if fi.module.Target().Os == android.Linux || fi.module.Target().Os == android.LinuxBionic {
					makeOs = "linux"
				}
				fmt.Fprintln(w, "LOCAL_MODULE_HOST_OS :=", makeOs)
				fmt.Fprintln(w, "LOCAL_IS_HOST_MODULE := true")
			}
		}
		if fi.jacocoReportClassesFile != nil {
			fmt.Fprintln(w, "LOCAL_SOONG_JACOCO_REPORT_CLASSES_JAR :=", fi.jacocoReportClassesFile.String())
		}
		switch fi.class {
		case javaSharedLib:
			// soong_java_prebuilt.mk sets LOCAL_MODULE_SUFFIX := .jar  Therefore
			// we need to remove the suffix from LOCAL_MODULE_STEM, otherwise
			// we will have foo.jar.jar
			fmt.Fprintln(w, "LOCAL_MODULE_STEM :=", strings.TrimSuffix(fi.stem(), ".jar"))
			if javaModule, ok := fi.module.(java.ApexDependency); ok {
				fmt.Fprintln(w, "LOCAL_SOONG_CLASSES_JAR :=", javaModule.ImplementationAndResourcesJars()[0].String())
				fmt.Fprintln(w, "LOCAL_SOONG_HEADER_JAR :=", javaModule.HeaderJars()[0].String())
			} else {
				fmt.Fprintln(w, "LOCAL_SOONG_CLASSES_JAR :=", fi.builtFile.String())
				fmt.Fprintln(w, "LOCAL_SOONG_HEADER_JAR :=", fi.builtFile.String())
			}
			fmt.Fprintln(w, "LOCAL_SOONG_DEX_JAR :=", fi.builtFile.String())
			fmt.Fprintln(w, "LOCAL_DEX_PREOPT := false")
			fmt.Fprintln(w, "include $(BUILD_SYSTEM)/soong_java_prebuilt.mk")
		case app:
			fmt.Fprintln(w, "LOCAL_CERTIFICATE :=", fi.certificate.AndroidMkString())
			// soong_app_prebuilt.mk sets LOCAL_MODULE_SUFFIX := .apk  Therefore
			// we need to remove the suffix from LOCAL_MODULE_STEM, otherwise
			// we will have foo.apk.apk
			fmt.Fprintln(w, "LOCAL_MODULE_STEM :=", strings.TrimSuffix(fi.stem(), ".apk"))
			if app, ok := fi.module.(*java.AndroidApp); ok {
				if jniCoverageOutputs := app.JniCoverageOutputs(); len(jniCoverageOutputs) > 0 {
					fmt.Fprintln(w, "LOCAL_PREBUILT_COVERAGE_ARCHIVE :=", strings.Join(jniCoverageOutputs.Strings(), " "))
				}
				if jniLibSymbols := app.JNISymbolsInstalls(modulePath); len(jniLibSymbols) > 0 {
					fmt.Fprintln(w, "LOCAL_SOONG_JNI_LIBS_SYMBOLS :=", jniLibSymbols.String())
				}
			}
			fmt.Fprintln(w, "include $(BUILD_SYSTEM)/soong_app_prebuilt.mk")
		case appSet:
			as, ok := fi.module.(*java.AndroidAppSet)
			if !ok {
				panic(fmt.Sprintf("Expected %s to be AndroidAppSet", fi.module))
			}
			fmt.Fprintln(w, "LOCAL_APK_SET_INSTALL_FILE :=", as.InstallFile())
			fmt.Fprintln(w, "LOCAL_APKCERTS_FILE :=", as.APKCertsFile().String())
			fmt.Fprintln(w, "include $(BUILD_SYSTEM)/soong_android_app_set.mk")
		case nativeSharedLib, nativeExecutable, nativeTest:
			fmt.Fprintln(w, "LOCAL_MODULE_STEM :=", fi.stem())
			if ccMod, ok := fi.module.(*cc.Module); ok {
				if ccMod.UnstrippedOutputFile() != nil {
					fmt.Fprintln(w, "LOCAL_SOONG_UNSTRIPPED_BINARY :=", ccMod.UnstrippedOutputFile().String())
				}
				ccMod.AndroidMkWriteAdditionalDependenciesForSourceAbiDiff(w)
				if ccMod.CoverageOutputFile().Valid() {
					fmt.Fprintln(w, "LOCAL_PREBUILT_COVERAGE_ARCHIVE :=", ccMod.CoverageOutputFile().String())
				}
			}
			fmt.Fprintln(w, "include $(BUILD_SYSTEM)/soong_cc_prebuilt.mk")
		default:
			fmt.Fprintln(w, "LOCAL_MODULE_STEM :=", fi.stem())
			if fi.builtFile == a.manifestPbOut && apexType == flattenedApex {
				if a.primaryApexType {
					// To install companion files (init_rc, vintf_fragments)
					// Copy some common properties of apexBundle to apex_manifest
					commonProperties := []string{
						"LOCAL_INIT_RC", "LOCAL_VINTF_FRAGMENTS",
					}
					for _, name := range commonProperties {
						if value, ok := apexAndroidMkData.Entries.EntryMap[name]; ok {
							fmt.Fprintln(w, name+" := "+strings.Join(value, " "))
						}
					}

					// Make apex_manifest.pb module for this APEX to override all other
					// modules in the APEXes being overridden by this APEX
					var patterns []string
					for _, o := range a.overridableProperties.Overrides {
						patterns = append(patterns, "%."+o+a.suffix)
					}
					if len(patterns) > 0 {
						fmt.Fprintln(w, "LOCAL_OVERRIDES_MODULES :=", strings.Join(patterns, " "))
					}
					if len(a.compatSymlinks) > 0 {
						// For flattened apexes, compat symlinks are attached to apex_manifest.json which is guaranteed for every apex
						postInstallCommands = append(postInstallCommands, a.compatSymlinks...)
					}
				}

				// File_contexts of flattened APEXes should be merged into file_contexts.bin
				fmt.Fprintln(w, "LOCAL_FILE_CONTEXTS :=", a.fileContexts)

				if len(postInstallCommands) > 0 {
					fmt.Fprintln(w, "LOCAL_POST_INSTALL_CMD :=", strings.Join(postInstallCommands, " && "))
				}
			}
			fmt.Fprintln(w, "include $(BUILD_PREBUILT)")
		}

		// m <module_name> will build <module_name>.<apex_name> as well.
		if fi.androidMkModuleName != moduleName && a.primaryApexType {
			fmt.Fprintf(w, ".PHONY: %s\n", fi.androidMkModuleName)
			fmt.Fprintf(w, "%s: %s\n", fi.androidMkModuleName, moduleName)
		}
	}
	return moduleNames
}

func (a *apexBundle) writeRequiredModules(w io.Writer) {
	var required []string
	var targetRequired []string
	var hostRequired []string
	for _, fi := range a.filesInfo {
		required = append(required, fi.requiredModuleNames...)
		targetRequired = append(targetRequired, fi.targetRequiredModuleNames...)
		hostRequired = append(hostRequired, fi.hostRequiredModuleNames...)
	}

	if len(required) > 0 {
		fmt.Fprintln(w, "LOCAL_REQUIRED_MODULES +=", strings.Join(required, " "))
	}
	if len(targetRequired) > 0 {
		fmt.Fprintln(w, "LOCAL_TARGET_REQUIRED_MODULES +=", strings.Join(targetRequired, " "))
	}
	if len(hostRequired) > 0 {
		fmt.Fprintln(w, "LOCAL_HOST_REQUIRED_MODULES +=", strings.Join(hostRequired, " "))
	}
}

func (a *apexBundle) androidMkForType() android.AndroidMkData {
	return android.AndroidMkData{
		Custom: func(w io.Writer, name, prefix, moduleDir string, data android.AndroidMkData) {
			moduleNames := []string{}
			apexType := a.properties.ApexType
			if a.installable() {
				apexName := proptools.StringDefault(a.properties.Apex_name, name)
				moduleNames = a.androidMkForFiles(w, name, apexName, moduleDir, data)
			}

			if apexType == flattenedApex {
				// Only image APEXes can be flattened.
				fmt.Fprintln(w, "\ninclude $(CLEAR_VARS)")
				fmt.Fprintln(w, "LOCAL_PATH :=", moduleDir)
				fmt.Fprintln(w, "LOCAL_MODULE :=", name+a.suffix)
				if len(moduleNames) > 0 {
					fmt.Fprintln(w, "LOCAL_REQUIRED_MODULES :=", strings.Join(moduleNames, " "))
				}
				a.writeRequiredModules(w)
				fmt.Fprintln(w, "include $(BUILD_PHONY_PACKAGE)")

			} else {
				fmt.Fprintln(w, "\ninclude $(CLEAR_VARS)")
				fmt.Fprintln(w, "LOCAL_PATH :=", moduleDir)
				fmt.Fprintln(w, "LOCAL_MODULE :=", name+a.suffix)
				fmt.Fprintln(w, "LOCAL_MODULE_CLASS := ETC") // do we need a new class?
				fmt.Fprintln(w, "LOCAL_PREBUILT_MODULE_FILE :=", a.outputFile.String())
				fmt.Fprintln(w, "LOCAL_MODULE_PATH :=", a.installDir.ToMakePath().String())
				fmt.Fprintln(w, "LOCAL_MODULE_STEM :=", name+apexType.suffix())
				fmt.Fprintln(w, "LOCAL_UNINSTALLABLE_MODULE :=", !a.installable())

				// Because apex writes .mk with Custom(), we need to write manually some common properties
				// which are available via data.Entries
				commonProperties := []string{
					"LOCAL_INIT_RC", "LOCAL_VINTF_FRAGMENTS",
					"LOCAL_PROPRIETARY_MODULE", "LOCAL_VENDOR_MODULE", "LOCAL_ODM_MODULE", "LOCAL_PRODUCT_MODULE", "LOCAL_SYSTEM_EXT_MODULE",
					"LOCAL_MODULE_OWNER",
				}
				for _, name := range commonProperties {
					if value, ok := data.Entries.EntryMap[name]; ok {
						fmt.Fprintln(w, name+" := "+strings.Join(value, " "))
					}
				}

				if len(a.overridableProperties.Overrides) > 0 {
					fmt.Fprintln(w, "LOCAL_OVERRIDES_MODULES :=", strings.Join(a.overridableProperties.Overrides, " "))
				}
				if len(moduleNames) > 0 {
					fmt.Fprintln(w, "LOCAL_REQUIRED_MODULES +=", strings.Join(moduleNames, " "))
				}
				if len(a.requiredDeps) > 0 {
					fmt.Fprintln(w, "LOCAL_REQUIRED_MODULES +=", strings.Join(a.requiredDeps, " "))
				}
				a.writeRequiredModules(w)
				var postInstallCommands []string
				if a.prebuiltFileToDelete != "" {
					postInstallCommands = append(postInstallCommands, "rm -rf "+
						filepath.Join(a.installDir.ToMakePath().String(), a.prebuiltFileToDelete))
				}
				// For unflattened apexes, compat symlinks are attached to apex package itself as LOCAL_POST_INSTALL_CMD
				postInstallCommands = append(postInstallCommands, a.compatSymlinks...)
				if len(postInstallCommands) > 0 {
					fmt.Fprintln(w, "LOCAL_POST_INSTALL_CMD :=", strings.Join(postInstallCommands, " && "))
				}

				if a.mergedNotices.Merged.Valid() {
					fmt.Fprintln(w, "LOCAL_NOTICE_FILE :=", a.mergedNotices.Merged.Path().String())
				}

				fmt.Fprintln(w, "include $(BUILD_PREBUILT)")

				if apexType == imageApex {
					fmt.Fprintln(w, "ALL_MODULES.$(my_register_name).BUNDLE :=", a.bundleModuleFile.String())
				}
				if len(a.lintReports) > 0 {
					fmt.Fprintln(w, "ALL_MODULES.$(my_register_name).LINT_REPORTS :=",
						strings.Join(a.lintReports.Strings(), " "))
				}

				if a.installedFilesFile != nil {
					goal := "checkbuild"
					distFile := name + "-installed-files.txt"
					fmt.Fprintln(w, ".PHONY:", goal)
					fmt.Fprintf(w, "$(call dist-for-goals,%s,%s:%s)\n",
						goal, a.installedFilesFile.String(), distFile)
				}
				for _, dist := range data.Entries.GetDistForGoals(a) {
					fmt.Fprintf(w, dist)
				}
			}
		}}
}