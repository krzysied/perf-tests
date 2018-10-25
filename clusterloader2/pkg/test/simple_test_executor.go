/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package test

import (
	"fmt"
	"io/ioutil"
	"math"
	"path"
	"time"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/perf-tests/clusterloader2/api"
	"k8s.io/perf-tests/clusterloader2/pkg/state"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	namePlaceholder  = "Name"
	indexPlaceholder = "Index"
)

type simpleTestExecutor struct{}

func createSimpleTestExecutor() TestExecutor {
	return &simpleTestExecutor{}
}

// ExecuteTest executes test based on provided configuration.
func (ste *simpleTestExecutor) ExecuteTest(ctx Context, conf *api.Config) *util.ErrorList {
	ctx.GetFramework().SetAutomanagedNamespacePrefix(fmt.Sprintf("test-%s", util.RandomDNS1123String(6)))
	glog.Infof("AutomanagedNamespacePrefix: %s", ctx.GetFramework().GetAutomanagedNamespacePrefix())
	defer cleanupResources(ctx)
	ctx.GetTuningSetFactory().Init(conf.TuningSets)
	automanagedNamespacesList, err := ctx.GetFramework().ListAutomanagedNamespaces()
	if err != nil {
		return util.NewErrorList(fmt.Errorf("automanaged namespaces listing failed: %v", err))
	}
	if len(automanagedNamespacesList) > 0 {
		return util.NewErrorList(fmt.Errorf("pre-existing automanaged namespaces found"))
	}
	err = ctx.GetFramework().CreateAutomanagedNamespaces(int(conf.AutomanagedNamespaces))
	if err != nil {
		return util.NewErrorList(fmt.Errorf("automanaged namespaces creation failed: %v", err))
	}

	errList := util.NewErrorList()
	for i := range conf.Steps {
		if stepErrList := ste.ExecuteStep(ctx, &conf.Steps[i]); !stepErrList.IsEmpty() {
			errList.Concat(stepErrList)
			if isErrsCritical(stepErrList) {
				return errList
			}
		}
	}

	for _, summary := range ctx.GetMeasurementManager().GetSummaries() {
		summaryText, err := summary.PrintSummary()
		if err != nil {
			errList.Append(fmt.Errorf("printing summary %s error: %v", summary.SummaryName(), err))
			continue
		}
		if ctx.GetClusterLoaderConfig().ReportDir == "" {
			glog.Infof("%v: %v", summary.SummaryName(), summaryText)
		} else {
			// TODO(krzysied): Remember to keep original filename style for backward compatibility.
			filePath := path.Join(ctx.GetClusterLoaderConfig().ReportDir, summary.SummaryName()+"_"+conf.Name+"_"+time.Now().Format(time.RFC3339)+".txt")
			if err := ioutil.WriteFile(filePath, []byte(summaryText), 0644); err != nil {
				errList.Append(fmt.Errorf("writing to file %v error: %v", filePath, err))
				continue
			}
		}
	}
	return errList
}

// ExecuteStep executes single test step based on provided step configuration.
func (ste *simpleTestExecutor) ExecuteStep(ctx Context, step *api.Step) *util.ErrorList {
	var wg wait.Group
	errList := util.NewErrorList()
	if len(step.Measurements) > 0 {
		for i := range step.Measurements {
			// index is created to make i value unchangeable during thread execution.
			index := i
			wg.Start(func() {
				err := ctx.GetMeasurementManager().Execute(step.Measurements[index].Method,
					step.Measurements[index].Identifier,
					step.Measurements[index].Params)
				if err != nil {
					errList.Append(fmt.Errorf("measurement call %s - %s error: %v", step.Measurements[index].Method, step.Measurements[index].Identifier, err))
				}
			})
		}
	} else {
		for i := range step.Phases {
			phase := &step.Phases[i]
			wg.Start(func() {
				if phaseErrList := ste.ExecutePhase(ctx, phase); !phaseErrList.IsEmpty() {
					errList.Concat(phaseErrList)
				}
			})
		}
	}
	wg.Wait()
	if step.Name != "" {
		glog.Infof("Step \"%s\" ended", step.Name)
	}
	return errList
}

// ExecutePhase executes single test phase based on provided phase configuration.
func (ste *simpleTestExecutor) ExecutePhase(ctx Context, phase *api.Phase) *util.ErrorList {
	// TODO: add tuning set
	errList := util.NewErrorList()
	nsList := createNamespacesList(ctx, phase.NamespaceRange)
	tuningSet, err := ctx.GetTuningSetFactory().CreateTuningSet(phase.TuningSet)
	if err != nil {
		return util.NewErrorList(fmt.Errorf("tuning set creation error: %v", err))
	}

	var actions []func()
	for namespaceIndex := range nsList {
		nsName := nsList[namespaceIndex]
		instancesStates := make([]*state.InstancesState, 0)
		// Updating state (DesiredReplicaCount) of every object in object bundle.
		for j := range phase.ObjectBundle {
			id, err := getIdentifier(ctx, &phase.ObjectBundle[j])
			if err != nil {
				errList.Append(err)
				return errList
			}
			instances, exists := ctx.GetState().GetNamespacesState().Get(nsName, id)
			if !exists {
				instances = &state.InstancesState{
					DesiredReplicaCount: 0,
					CurrentReplicaCount: 0,
					Object:              phase.ObjectBundle[j],
				}
			}
			instances.DesiredReplicaCount = phase.ReplicasPerNamespace
			ctx.GetState().GetNamespacesState().Set(nsName, id, instances)
			instancesStates = append(instancesStates, instances)
		}

		// Calculating maximal replica count of objects from object bundle.
		var maxCurrentReplicaCount int32
		for j := range instancesStates {
			if instancesStates[j].CurrentReplicaCount > maxCurrentReplicaCount {
				maxCurrentReplicaCount = instancesStates[j].CurrentReplicaCount
			}
		}

		// Deleting objects with index greater or equal requested replicas per namespace number.
		// Objects will be deleted in reversed order.
		for replicaCounter := phase.ReplicasPerNamespace; replicaCounter < maxCurrentReplicaCount; replicaCounter++ {
			replicaIndex := replicaCounter
			actions = append(actions, func() {
				for j := len(phase.ObjectBundle) - 1; j >= 0; j-- {
					if replicaIndex < instancesStates[j].CurrentReplicaCount {
						if objectErrList := ste.ExecuteObject(ctx, &phase.ObjectBundle[j], nsName, replicaIndex, DELETE_OBJECT); !objectErrList.IsEmpty() {
							errList.Concat(objectErrList)
						}
					}
				}
			})
		}

		// Calculating minimal replica count of objects from object bundle.
		// If there is update operation to be executed, minimal replica count is set to zero.
		minCurrentReplicaCount := int32(math.MaxInt32)
		for j := range instancesStates {
			if instancesStates[j].CurrentReplicaCount == phase.ReplicasPerNamespace {
				minCurrentReplicaCount = 0
				break
			}
			if instancesStates[j].CurrentReplicaCount < minCurrentReplicaCount {
				minCurrentReplicaCount = instancesStates[j].CurrentReplicaCount
			}
		}
		// Handling for update/create objects.
		for replicaCounter := minCurrentReplicaCount; replicaCounter < phase.ReplicasPerNamespace; replicaCounter++ {
			replicaIndex := replicaCounter
			actions = append(actions, func() {
				for j := range phase.ObjectBundle {
					if instancesStates[j].CurrentReplicaCount == phase.ReplicasPerNamespace {
						if objectErrList := ste.ExecuteObject(ctx, &phase.ObjectBundle[j], nsName, replicaIndex, PATCH_OBJECT); !objectErrList.IsEmpty() {
							errList.Concat(objectErrList)
							// If error then skip this bundle
							break
						}
					} else if replicaIndex >= instancesStates[j].CurrentReplicaCount {
						if objectErrList := ste.ExecuteObject(ctx, &phase.ObjectBundle[j], nsName, replicaIndex, CREATE_OBJECT); !objectErrList.IsEmpty() {
							errList.Concat(objectErrList)
							// If error then skip this bundle
							break
						}
					}
				}
			})
		}
		// Updating state (CurrentReplicaCount) of every object in object bundle.
		defer func() {
			for j := range phase.ObjectBundle {
				id, _ := getIdentifier(ctx, &phase.ObjectBundle[j])
				instancesStates[j].CurrentReplicaCount = instancesStates[j].DesiredReplicaCount
				ctx.GetState().GetNamespacesState().Set(nsName, id, instancesStates[j])
			}
		}()

	}
	tuningSet.Execute(actions)
	return errList
}

// ExecuteObject executes single test object operation based on provided object configuration.
func (ste *simpleTestExecutor) ExecuteObject(ctx Context, object *api.Object, namespace string, replicaIndex int32, operation OperationType) *util.ErrorList {
	objName := fmt.Sprintf("%v-%d", object.Basename, replicaIndex)
	var err error
	var obj *unstructured.Unstructured
	switch operation {
	case CREATE_OBJECT, PATCH_OBJECT:
		mapping := make(map[string]interface{})
		if object.TemplateFillMap != nil {
			util.CopyMap(object.TemplateFillMap, mapping)
		}
		mapping[namePlaceholder] = objName
		mapping[indexPlaceholder] = replicaIndex
		obj, err = ctx.GetTemplateProvider().TemplateToObject(object.ObjectTemplatePath, mapping)
		if err != nil {
			return util.NewErrorList(fmt.Errorf("reading template (%v) error: %v", object.ObjectTemplatePath, err))
		}
	case DELETE_OBJECT:
		obj, err = ctx.GetTemplateProvider().RawToObject(object.ObjectTemplatePath)
		if err != nil {
			return util.NewErrorList(fmt.Errorf("reading template (%v) for deletion error: %v", object.ObjectTemplatePath, err))
		}
	default:
		return util.NewErrorList(fmt.Errorf("unsupported operation %v for namespace %v object %v", operation, namespace, objName))
	}
	gvk := obj.GroupVersionKind()

	errList := util.NewErrorList()
	switch operation {
	case CREATE_OBJECT:
		if newObj, err := ctx.GetFramework().CreateObject(namespace, objName, obj); err != nil {
			errList.Append(fmt.Errorf("namespace %v object %v creation error: %v", namespace, objName, err))
		} else {
			err = updateResourceVersion(ctx, newObj)
			if err != nil {
				errList.Append(fmt.Errorf("namespace %v object %v resource version paring error: %v", namespace, objName, err))
			}
		}
	case PATCH_OBJECT:
		if newObj, err := ctx.GetFramework().PatchObject(namespace, objName, obj); err != nil {
			errList.Append(fmt.Errorf("namespace %v object %v updating error: %v", namespace, objName, err))
		} else {
			err = updateResourceVersion(ctx, newObj)
			if err != nil {
				errList.Append(fmt.Errorf("namespace %v object %v resource version paring error: %v", namespace, objName, err))
			}
		}
	case DELETE_OBJECT:
		if err := ctx.GetFramework().DeleteObject(gvk, namespace, objName); err != nil {
			errList.Append(fmt.Errorf("namespace %v object %v deletion error: %v", namespace, objName, err))
		}
	}
	return errList
}

func getIdentifier(ctx Context, object *api.Object) (state.InstancesIdentifier, error) {
	objName := fmt.Sprintf("%v-%d", object.Basename, 0)
	mapping := make(map[string]interface{})
	if object.TemplateFillMap != nil {
		util.CopyMap(object.TemplateFillMap, mapping)
	}
	mapping[namePlaceholder] = objName
	mapping[indexPlaceholder] = 0
	obj, err := ctx.GetTemplateProvider().RawToObject(object.ObjectTemplatePath)
	if err != nil {
		return state.InstancesIdentifier{}, fmt.Errorf("reading template (%v) for identifier error: %v", object.ObjectTemplatePath, err)
	}
	gvk := obj.GroupVersionKind()
	return state.InstancesIdentifier{
		Basename:   object.Basename,
		ObjectKind: gvk.Kind,
		ApiGroup:   gvk.Group,
	}, nil
}

func createNamespacesList(ctx Context, namespaceRange *api.NamespaceRange) []string {
	if namespaceRange == nil {
		// Returns "" which represents cluster level.
		return []string{""}
	}

	nsList := make([]string, 0)
	nsBasename := ctx.GetFramework().GetAutomanagedNamespacePrefix()
	if namespaceRange.Basename != nil {
		nsBasename = *namespaceRange.Basename
	}

	for i := namespaceRange.Min; i <= namespaceRange.Max; i++ {
		nsList = append(nsList, fmt.Sprintf("%v-%d", nsBasename, i))
	}
	return nsList
}

func updateResourceVersion(ctx Context, obj *unstructured.Unstructured) error {
	identifier := state.ResourceTypeIdentifier{
		ObjectKind: obj.GetKind(),
		ApiGroup:   obj.GetAPIVersion(),
	}
	return ctx.GetState().GetResourcesVersionState().Set(identifier, obj.GetResourceVersion())
}

func isErrsCritical(*util.ErrorList) bool {
	// TODO: define critical errors
	return false
}

func cleanupResources(ctx Context) {
	cleanupStartTime := time.Now()
	if errList := ctx.GetFramework().DeleteAutomanagedNamespaces(); !errList.IsEmpty() {
		glog.Errorf("Resource cleanup error: %v", errList.String())
		return
	}
	glog.Infof("Resources cleanup time: %v", time.Since(cleanupStartTime))
}
