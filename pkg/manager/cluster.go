package manager

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sjson "k8s.io/apimachinery/pkg/runtime/serializer/json"

	"github.com/rancher/support-bundle-kit/pkg/manager/collectors"
	"github.com/rancher/support-bundle-kit/pkg/utils"
)

type Cluster struct {
	sbm *SupportBundleManager
}

func NewCluster(ctx context.Context, sbm *SupportBundleManager) *Cluster {
	return &Cluster{
		sbm: sbm,
	}
}

func (c *Cluster) GenerateClusterBundle(bundleDir string) (string, error) {
	logrus.Debug("Generating cluster bundle...")
	namespace, err := c.sbm.k8s.GetNamespace(c.sbm.PodNamespace)
	if err != nil {
		return "", errors.Wrap(err, "cannot get deployed namespace")
	}
	kubeVersion, err := c.sbm.k8s.GetKubernetesVersion()
	if err != nil {
		return "", errors.Wrap(err, "cannot get kubernetes version")
	}

	sb, err := c.sbm.state.GetSupportBundle(c.sbm.PodNamespace, c.sbm.BundleName)
	if err != nil {
		return "", errors.Wrap(err, "cannot get support bundle")
	}

	bundleMeta := &BundleMeta{
		BundleName:           sb.Name,
		BundleVersion:        BundleVersion,
		KubernetesVersion:    kubeVersion.GitVersion,
		ProjectNamespaceUUID: string(namespace.UID),
		BundleCreatedAt:      utils.Now(),
		IssueURL:             sb.Spec.IssueURL,
		IssueDescription:     sb.Spec.Description,
	}

	bundleName := fmt.Sprintf("supportbundle_%s_%s.zip",
		bundleMeta.ProjectNamespaceUUID,
		strings.Replace(bundleMeta.BundleCreatedAt, ":", "-", -1))

	errLog, err := os.Create(filepath.Join(bundleDir, "bundleGenerationError.log"))
	if err != nil {
		logrus.Errorf("Failed to create bundle generation log")
		return "", err
	}
	defer errLog.Close()

	metaFile := filepath.Join(bundleDir, "metadata.yaml")
	encodeToYAMLFile(bundleMeta, metaFile, errLog)

	yamlsDir := filepath.Join(bundleDir, "yamls")
	var modules []interface{}
	for _, moduleName := range c.sbm.BundleCollectors {
		module := collectors.InitModuleCollector(moduleName, yamlsDir, c.sbm.Namespaces, c.sbm.discovery, c.matchesExcludeResources, encodeToYAMLFile, errLog)
		modules = append(modules, module)
	}
	collectors.GetAllSupportBundleYAMLs(modules)

	logsDir := filepath.Join(bundleDir, "logs")
	c.generateSupportBundleLogs(logsDir, errLog)

	return bundleName, nil
}

// matchesExcludeResources returns true if given resource group version mathces our ExcludeResources list.
func (c *Cluster) matchesExcludeResources(gv schema.GroupVersion, resource metav1.APIResource) bool {
	for _, excludeResource := range c.sbm.ExcludeResources {
		if gv.Group == excludeResource.Group && resource.Name == excludeResource.Resource {
			return true
		}
	}
	return false
}

func encodeToYAMLFile(obj interface{}, path string, errLog io.Writer) {
	var err error
	defer func() {
		if err != nil {
			fmt.Fprintf(errLog, "Support Bundle: failed to generate %v: %v\n", path, err)
		}
	}()
	err = os.MkdirAll(filepath.Dir(path), os.FileMode(0755))
	if err != nil {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()

	switch v := obj.(type) {
	case runtime.Object:
		serializer := k8sjson.NewSerializerWithOptions(k8sjson.DefaultMetaFactory, nil, nil, k8sjson.SerializerOptions{
			// only a subset of yaml that matches JSON is generated
			// https://github.com/kubernetes/apimachinery/blob/1af25b613b6482b465c4bf23501a9b02acdb3c0c/pkg/runtime/serializer/json/json.go#L86
			Yaml:   true,
			Pretty: true,
			Strict: true,
		})
		if err = serializer.Encode(v, f); err != nil {
			return
		}
	default:
		encoder := yaml.NewEncoder(f)
		if err = encoder.Encode(obj); err != nil {
			return
		}
		if err = encoder.Close(); err != nil {
			return
		}
	}
}

type GetRuntimeObjectListFunc func() (runtime.Object, error)

func (c *Cluster) generateSupportBundleLogs(logsDir string, errLog io.Writer) {
	namespaces := []string{"default", "kube-system", "cattle-system"}
	namespaces = append(namespaces, c.sbm.Namespaces...)

	for _, ns := range namespaces {
		list, err := c.sbm.k8s.GetAllPodsList(ns)
		if err != nil {
			fmt.Fprintf(errLog, "Support bundle: cannot get pod list: %v\n", err)
			return
		}
		podList, ok := list.(*corev1.PodList)
		if !ok {
			fmt.Fprintf(errLog, "BUG: Support bundle: didn't get pod list\n")
			return
		}
		for _, pod := range podList.Items {
			podName := pod.Name
			podDir := filepath.Join(logsDir, ns, podName)
			for _, container := range pod.Spec.Containers {
				req := c.sbm.k8s.GetPodContainerLogRequest(ns, podName, container.Name)
				logFileName := filepath.Join(podDir, container.Name+".log")
				stream, err := req.Stream(c.sbm.context)
				if err != nil {
					fmt.Fprintf(errLog, "BUG: Support bundle: cannot get log for pod %v container %v: %v\n",
						podName, container.Name, err)
					continue
				}
				streamLogToFile(stream, logFileName, errLog)
				stream.Close()
			}
		}
	}
}

func streamLogToFile(logStream io.ReadCloser, path string, errLog io.Writer) {
	var err error
	defer func() {
		if err != nil {
			fmt.Fprintf(errLog, "Support Bundle: failed to generate %v: %v\n", path, err)
		}
	}()
	err = os.MkdirAll(filepath.Dir(path), os.FileMode(0755))
	if err != nil {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	_, err = io.Copy(f, logStream)
	if err != nil {
		return
	}
}
