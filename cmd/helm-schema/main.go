package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"

	"github.com/dadav/helm-schema/pkg/chart"
	"github.com/dadav/helm-schema/pkg/schema"
)

func searchFiles(chartSearchRoot, startPath, fileName string, dependenciesFilter map[string]bool, queue chan<- string, errs chan<- error) {
	defer close(queue)
	err := filepath.Walk(startPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			errs <- err
			return nil
		}

		if !info.IsDir() && info.Name() == fileName {
			if filepath.Dir(path) == chartSearchRoot {
				queue <- path
				return nil
			}

			if len(dependenciesFilter) > 0 {
				chartData, err := os.ReadFile(path)
				if err != nil {
					errs <- fmt.Errorf("failed to read Chart.yaml at %s: %w", path, err)
					return nil
				}

				var chart chart.ChartFile
				if err := yaml.Unmarshal(chartData, &chart); err != nil {
					errs <- fmt.Errorf("failed to parse Chart.yaml at %s: %w", path, err)
					return nil
				}

				if dependenciesFilter[chart.Name] {
					queue <- path
				}
			} else {
				queue <- path
			}
		}

		return nil
	})
	if err != nil {
		errs <- err
	}
}

func exec(cmd *cobra.Command, _ []string) error {
	configureLogging()

	var skipAutoGeneration, valueFileNames []string

	chartSearchRoot := viper.GetString("chart-search-root")
	dryRun := viper.GetBool("dry-run")
	noDeps := viper.GetBool("no-dependencies")
	addSchemaReference := viper.GetBool("add-schema-reference")
	keepFullComment := viper.GetBool("keep-full-comment")
	helmDocsCompatibilityMode := viper.GetBool("helm-docs-compatibility-mode")
	uncomment := viper.GetBool("uncomment")
	outFile := viper.GetString("output-file")
	dontRemoveHelmDocsPrefix := viper.GetBool("dont-strip-helm-docs-prefix")
	appendNewline := viper.GetBool("append-newline")
	dependenciesFilter := viper.GetStringSlice("dependencies-filter")
	dependenciesFilterMap := make(map[string]bool)
	dontAddGlobal := viper.GetBool("dont-add-global")
	for _, dep := range dependenciesFilter {
		dependenciesFilterMap[dep] = true
	}
	if err := viper.UnmarshalKey("value-files", &valueFileNames); err != nil {
		return err
	}
	if err := viper.UnmarshalKey("skip-auto-generation", &skipAutoGeneration); err != nil {
		return err
	}
	workersCount := runtime.NumCPU() * 2

	skipConfig, err := schema.NewSkipAutoGenerationConfig(skipAutoGeneration)
	if err != nil {
		return err
	}

	queue := make(chan string)
	resultsChan := make(chan schema.Result)
	results := []*schema.Result{}
	errs := make(chan error)
	done := make(chan struct{})

	go searchFiles(chartSearchRoot, chartSearchRoot, "Chart.yaml", dependenciesFilterMap, queue, errs)

	wg := sync.WaitGroup{}
	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	for i := 0; i < workersCount; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()
			schema.Worker(
				dryRun,
				uncomment,
				addSchemaReference,
				keepFullComment,
				helmDocsCompatibilityMode,
				dontRemoveHelmDocsPrefix,
				dontAddGlobal,
				valueFileNames,
				skipConfig,
				outFile,
				queue,
				resultsChan,
			)
		}()
	}

loop:
	for {
		select {
		case err := <-errs:
			log.Error(err)
		case res := <-resultsChan:
			results = append(results, &res)
		case <-done:
			break loop

		}
	}

	if !noDeps {
		results, err = schema.TopoSort(results)
		if err != nil {
			if _, ok := err.(*schema.CircularError); !ok {
				log.Errorf("Error while sorting results: %s", err)
				return err
			} else {
				log.Warnf("Could not sort results: %s", err)
			}
		}
	}

	conditionsToPatch := make(map[string][]string)
	if !noDeps {
		for _, result := range results {
			if len(result.Errors) > 0 {
				continue
			}
			for _, dep := range result.Chart.Dependencies {
				if len(dependenciesFilterMap) > 0 && !dependenciesFilterMap[dep.Name] {
					continue
				}

				if dep.Condition != "" {
					conditionKeys := strings.Split(dep.Condition, ".")
					conditionsToPatch[conditionKeys[0]] = conditionKeys[1:]
				}
			}
		}
	}

	chartNameToResult := make(map[string]*schema.Result)
	foundErrors := false

	for _, result := range results {
		if len(result.Errors) > 0 {
			foundErrors = true
			if result.Chart != nil {
				log.Errorf(
					"Found %d errors while processing the chart %s (%s)",
					len(result.Errors),
					result.Chart.Name,
					result.ChartPath,
				)
			} else {
				log.Errorf("Found %d errors while processing the chart %s", len(result.Errors), result.ChartPath)
			}
			for _, err := range result.Errors {
				log.Error(err)
			}
			continue
		}

		log.Debugf("Processing result for chart: %s (%s)", result.Chart.Name, result.ChartPath)
		if !noDeps {
			chartNameToResult[result.Chart.Name] = result
			log.Debugf("Stored chart %s in chartNameToResult", result.Chart.Name)

			if patch, ok := conditionsToPatch[result.Chart.Name]; ok {
				schemaToPatch := &result.Schema
				lastIndex := len(patch) - 1
				for i, key := range patch {
					if alreadyPresentSchema, ok := schemaToPatch.Properties[key]; !ok {
						log.Debugf(
							"Patching conditional field \"%s\" into schema of chart %s",
							key,
							result.Chart.Name,
						)
						if i == lastIndex {
							schemaToPatch.Properties[key] = &schema.Schema{
								Type:        []string{"boolean"},
								Title:       key,
								Description: "Conditional property used in parent chart",
							}
						} else {
							schemaToPatch.Properties[key] = &schema.Schema{Type: []string{"object"}, Title: key}
							schemaToPatch = schemaToPatch.Properties[key]
						}
					} else {
						schemaToPatch = alreadyPresentSchema
					}
				}
			}

			for _, dep := range result.Chart.Dependencies {
				if len(dependenciesFilterMap) > 0 && !dependenciesFilterMap[dep.Name] {
					continue
				}

				if dep.Name != "" {
					if dependencyResult, ok := chartNameToResult[dep.Name]; ok {
						log.Debugf(
							"Found chart of dependency %s (%s)",
							dependencyResult.Chart.Name,
							dependencyResult.ChartPath,
						)
						depSchema := schema.Schema{
							Type:        []string{"object"},
							Title:       dep.Name,
							Description: dependencyResult.Chart.Description,
							Properties:  dependencyResult.Schema.Properties,
						}
						isParentLibraryChart := result.Chart.Type == "library"
						depSchema.DisableRequiredProperties(isParentLibraryChart)

						if (dependencyResult.Chart.Type == "library" && dep.ImportValues == nil) {
							log.Debugf("Dependency %s is a library chart, merging values into parent", dep.Name)
							for k, v := range dependencyResult.Schema.Properties {
								if _, ok := result.Schema.Properties[k]; !ok {
									result.Schema.Properties[k] = v
								}
							}
							continue
						} else if dep.Alias != "" && dep.ImportValues == nil {
							result.Schema.Properties[dep.Alias] = &depSchema
						} else if dep.ImportValues != nil {
							log.Debugf("Dependency %s has import-values, merging those values and ignoring others", dep.Name)
							for _, importValue := range dep.ImportValues {
									if importValue.Child != "" && importValue.Parent != "" {
											// Map format: {child: childPath, parent: parentPath}
											log.Debugf("  Importing values from %s to %s", importValue.Child, importValue.Parent)

											childParts := strings.Split(importValue.Child, ".")
											parentParts := strings.Split(importValue.Parent, ".")

											// Find schema at source path in dependency schema
											var sourceSchema *schema.Schema
											currentSchema := &depSchema
											for i, part := range childParts {
													if i == len(childParts)-1 {
															if prop, ok := currentSchema.Properties[part]; ok {
																	sourceSchema = prop
															} else {
																	log.Warnf("Source path %s not found in dependency %s", importValue.Child, dep.Name)
																	break
															}
													} else {
															if prop, ok := currentSchema.Properties[part]; ok {
																	currentSchema = prop
															} else {
																	log.Warnf("Source path %s not found in dependency %s", importValue.Child, dep.Name)
																	break
															}
													}
											}

											// Insert schema at target path
											if sourceSchema != nil {
													// Special case for importing to root (parent = ".")
													if importValue.Parent == "." {
															log.Debugf("  Importing %s to root level", importValue.Child)
															if result.Schema.Properties == nil {
																	result.Schema.Properties = make(map[string]*schema.Schema)
															}

															// If it's an object with properties, merge those properties
															// with what's already in the root schema taking precedence
															if len(sourceSchema.Type) > 0 &&
																sourceSchema.Type[0] == "object" && sourceSchema.Properties != nil {
																	for k, v := range sourceSchema.Properties {
																			if _, exists := result.Schema.Properties[k]; !exists {
																					result.Schema.Properties[k] = v
																					log.Debugf("    Added property %s to root", k)
																			} else {
																					log.Warnf("    Property %s already exists at root, merging", k)
																					deepMergeProperties(v, result.Schema.Properties[k], k)
																			}
																	}
															} else {
																	// If it's a leaf property, we can't merge it at the root
																	log.Warnf("  Cannot merge non-object property %s to root level", importValue.Child)
															}

															// If the source schema has any required properties, we need to add them to the root
															if len(sourceSchema.Required.Strings) > 0 {
																for _, req := range sourceSchema.Required.Strings {
																	exists := false
																	for _, existingReq := range result.Schema.Required.Strings {
																		if existingReq == req {
																			exists = true
																			break
																		}
																	}
																	if !exists {
																		result.Schema.Required.Strings = append(result.Schema.Required.Strings, req)
																		log.Debugf("    Added required property %s to root", req)
																	} else {
																		log.Warnf("    Required property %s already exists at root, skipping", req)
																	}
																}
															}

															log.Debugf("  Successfully imported %s to root level", importValue.Child)
													} else {
															// Regular case - insert at specified path
															currentSchema := &result.Schema
															for i, part := range parentParts {
																	if i == len(parentParts)-1 {
																			// Target path found, add the source schema here
																			if currentSchema.Properties == nil {
																					currentSchema.Properties = make(map[string]*schema.Schema)
																			}
																			currentSchema.Properties[part] = sourceSchema
																	} else {
																			// Create intermediate objects if needed
																			if currentSchema.Properties == nil {
																					currentSchema.Properties = make(map[string]*schema.Schema)
																			}

																			if _, ok := currentSchema.Properties[part]; !ok {
																					currentSchema.Properties[part] = &schema.Schema{
																							Type:       []string{"object"},
																							Properties: make(map[string]*schema.Schema),
																							Title:      part,
																					}
																			}
																			currentSchema = currentSchema.Properties[part]
																	}
															}
															log.Debugf("  Successfully imported %s to %s", importValue.Child, importValue.Parent)
													}
											}
									} else {
											// String format or other format we don't support yet
											log.Warnf("Unsupported import-values format in dependency %s. Please use parent/child format.", dep.Name)
									}
							}
							schema.FixRequiredProperties(&result.Schema)
						} else {
							result.Schema.Properties[dep.Name] = &depSchema
						}

					} else {
						log.Warnf("Dependency (%s->%s) specified but no schema found. If you want to create jsonschemas for external dependencies, you need to run helm dependency build & untar the charts.", result.Chart.Name, dep.Name)
					}
				} else {
					log.Warnf("Dependency without name found (checkout %s).", result.ChartPath)
				}
			}
		}

		jsonStr, err := result.Schema.ToJson()
		if err != nil {
			log.Error(err)
			continue
		}

		if appendNewline {
			jsonStr = append(jsonStr, '\n')
		}

		if dryRun {
			log.Infof("Printing jsonschema for %s chart (%s)", result.Chart.Name, result.ChartPath)
			if appendNewline {
				fmt.Printf("%s", jsonStr)
			} else {
				fmt.Printf("%s\n", jsonStr)
			}
		} else {
			chartBasePath := filepath.Dir(result.ChartPath)
			if err := os.WriteFile(filepath.Join(chartBasePath, outFile), jsonStr, 0644); err != nil {
				errs <- err
				continue
			}
		}
	}
	if foundErrors {
		return errors.New("some errors were found")
	}
	return nil
}

// deepMergeProperties merges properties from source into target
// with target properties taking precedence in conflicts
func deepMergeProperties(source *schema.Schema, target *schema.Schema, path string) {
    for k, srcProp := range source.Properties {
        if targetProp, exists := target.Properties[k]; exists {
            // Check if both are objects with properties - only then do deep merge
            isTargetObject := targetProp.Type != nil && len(targetProp.Type) > 0 &&
                            targetProp.Type[0] == "object" && targetProp.Properties != nil
            isSourceObject := srcProp.Type != nil && len(srcProp.Type) > 0 &&
                            srcProp.Type[0] == "object" && srcProp.Properties != nil

            if isTargetObject && isSourceObject {
                // Recursively merge properties
                log.Debugf("    Deep merging nested object property %s", k)
                deepMergeProperties(srcProp, targetProp, path+"."+k)
            } else {
                log.Warnf("    Property %s already exists at root, keeping existing definition", k)
            }
        } else {
            // Property doesn't exist in target, so add it
            target.Properties[k] = srcProp
            log.Debugf("    Added property %s to root", k)
        }
    }
}

func main() {
	command, err := newCommand(exec)
	if err != nil {
		log.Errorf("Failed to create the CLI commander: %s", err)
		os.Exit(1)
	}

	if err := command.Execute(); err != nil {
		log.Errorf("Execution error: %s", err)
		os.Exit(1)
	}
}
