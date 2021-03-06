/*
Copyright IBM Corporation 2020

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

package apiresourceset

import (
	"io/ioutil"

	"github.com/konveyor/move2kube/internal/apiresource"
	"github.com/konveyor/move2kube/internal/common"
	irtypes "github.com/konveyor/move2kube/internal/types"
	plantypes "github.com/konveyor/move2kube/types/plan"
	okdapi "github.com/openshift/api"
	log "github.com/sirupsen/logrus"
	tektonscheme "github.com/tektoncd/pipeline/pkg/client/clientset/versioned/scheme"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	k8sapischeme "k8s.io/client-go/kubernetes/scheme"
)

// K8sAPIResourceSet for handling K8s related resources
type K8sAPIResourceSet struct {
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

// GetScheme returns K8s scheme
func (*K8sAPIResourceSet) GetScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	must(okdapi.Install(scheme))
	must(okdapi.InstallKube(scheme))
	must(k8sapischeme.AddToScheme(scheme))
	must(tektonscheme.AddToScheme(scheme))
	return scheme
}

// CreateAPIResources converts IR to runtime objects
func (k8sAPIResourceSet *K8sAPIResourceSet) CreateAPIResources(oldir irtypes.IR) []runtime.Object {
	ir := irtypes.NewEnhancedIRFromIR(oldir)
	targetObjs := []runtime.Object{}
	ignoredObjs := ir.CachedObjects
	for _, apiResource := range k8sAPIResourceSet.getAPIResources(ir) {
		apiResource.SetClusterContext(ir.TargetClusterSpec)
		resourceIgnoredObjs := apiResource.LoadResources(ir.CachedObjects, ir)
		ignoredObjs = intersection(ignoredObjs, resourceIgnoredObjs)
		resourceObjs := apiResource.GetUpdatedResources(ir)
		targetObjs = append(targetObjs, resourceObjs...)
	}
	targetObjs = append(targetObjs, ignoredObjs...)
	return targetObjs
}

func (k8sAPIResourceSet *K8sAPIResourceSet) getAPIResources(ir irtypes.EnhancedIR) []apiresource.APIResource {
	return []apiresource.APIResource{
		{
			Scheme:       k8sAPIResourceSet.GetScheme(),
			IAPIResource: &apiresource.Deployment{Cluster: ir.TargetClusterSpec},
		},
		{
			Scheme:       k8sAPIResourceSet.GetScheme(),
			IAPIResource: &apiresource.Storage{Cluster: ir.TargetClusterSpec},
		},
		{
			Scheme:       k8sAPIResourceSet.GetScheme(),
			IAPIResource: &apiresource.Service{Cluster: ir.TargetClusterSpec},
		},
		{
			Scheme:       k8sAPIResourceSet.GetScheme(),
			IAPIResource: &apiresource.ImageStream{Cluster: ir.TargetClusterSpec},
		},
		{
			Scheme:       k8sAPIResourceSet.GetScheme(),
			IAPIResource: &apiresource.NetworkPolicy{Cluster: ir.TargetClusterSpec},
		},
	}
}

// GetServiceOptions analyses a directory and returns possible plan services
func (k8sAPIResourceSet *K8sAPIResourceSet) GetServiceOptions(inputPath string, plan plantypes.Plan) ([]plantypes.Service, error) {
	services := []plantypes.Service{}
	//TODO: Should we add service analysis too, to get service name?

	codecs := serializer.NewCodecFactory(k8sAPIResourceSet.GetScheme())

	filePaths, err := common.GetFilesByExt(inputPath, []string{".yml", ".yaml"})
	if err != nil {
		log.Errorf("Unable to fetch yaml files at path %q Error: %q", inputPath, err)
		return services, err
	}
	for _, filePath := range filePaths {
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			log.Debugf("Failed to read the yaml file at path %q Error: %q", filePath, err)
			continue
		}
		docs, err := common.SplitYAML(data)
		if err != nil {
			log.Errorf("Failed to split the file at path %q into YAML documents. Error: %q", filePath, err)
			continue
		}
		for _, doc := range docs {
			// filter to get only valid k8s Deployments
			obj, _, err := codecs.UniversalDeserializer().Decode(doc, nil, nil)
			if err != nil {
				continue
			}
			name, _, err := new(apiresource.Deployment).GetNameAndPodSpec(obj)
			if err != nil {
				continue
			}
			service := newK8sService(name)
			service.SourceArtifacts[plantypes.K8sFileArtifactType] = []string{filePath}
			services = append(services, service)
		}
	}
	return services, nil
}

// Translate translates plan services to IR
func (k8sAPIResourceSet *K8sAPIResourceSet) Translate(services []plantypes.Service, plan plantypes.Plan) (irtypes.IR, error) {
	ir := irtypes.NewIR(plan)
	codecs := serializer.NewCodecFactory(k8sAPIResourceSet.GetScheme())

	for _, service := range services {
		if len(service.SourceArtifacts[plantypes.K8sFileArtifactType]) == 0 {
			log.Warnf("No k8s artifacts found in service %s", service.ServiceName)
			continue
		}
		filePath := service.SourceArtifacts[plantypes.K8sFileArtifactType][0] // TODO: what about the other k8s files?
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			log.Errorf("Unable to read the k8s file at path %q Error: %q", filePath, err)
			continue
		}
		docs, err := common.SplitYAML(data)
		if err != nil {
			log.Errorf("Failed to split the k8s file at path %q into YAML documents. Error: %q", filePath, err)
			continue
		}
		for i, doc := range docs {
			obj, _, err := codecs.UniversalDeserializer().Decode(doc, nil, nil)
			if err != nil {
				log.Errorf("Failed to decode document %d of file at path %q as a k8s resource. Error: %q", i, filePath, err)
				continue
			}
			name, podSpec, err := new(apiresource.Deployment).GetNameAndPodSpec(obj)
			if err != nil {
				continue
			}
			if name != service.ServiceName {
				continue
			}
			irService := irtypes.NewServiceFromPlanService(service)
			irService.PodSpec = podSpec
			for _, container := range podSpec.Containers {
				for _, port := range container.Ports {
					podPort := irtypes.Port{Name: port.Name, Number: port.ContainerPort}
					servicePort := podPort
					irService.AddPortForwarding(servicePort, podPort)
				}
			}
			ir.Services[service.ServiceName] = irService
			break
		}
	}
	return ir, nil
}

func newK8sService(serviceName string) plantypes.Service {
	service := plantypes.NewService(serviceName, plantypes.Kube2KubeTranslation)
	service.ContainerBuildType = plantypes.ReuseContainerBuildTypeValue
	service.AddSourceType(plantypes.K8sSourceTypeValue)
	service.UpdateContainerBuildPipeline = false
	service.UpdateDeployPipeline = true
	return service
}
