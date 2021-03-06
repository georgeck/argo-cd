// Package kube provides helper utilities common for kubernetes

package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"sync"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/api/equality"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	listVerb             = "list"
	deleteVerb           = "delete"
	deleteCollectionVerb = "deletecollection"
)

var (
	// location to use for generating temporary files, such as the ca.crt needed by kubectl
	kubectlTempDir string
)

func init() {
	fileInfo, err := os.Stat("/dev/shm")
	if err == nil && fileInfo.IsDir() {
		kubectlTempDir = "/dev/shm"
	}
}

// TestConfig tests to make sure the REST config is usable
func TestConfig(config *rest.Config) error {
	kubeclientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("REST config invalid: %s", err)
	}
	_, err = kubeclientset.ServerVersion()
	if err != nil {
		return fmt.Errorf("REST config invalid: %s", err)
	}
	return nil
}

// ToUnstructured converts a concrete K8s API type to a un unstructured object
func ToUnstructured(obj interface{}) (*unstructured.Unstructured, error) {
	uObj, err := runtime.NewTestUnstructuredConverter(equality.Semantic).ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: uObj}, nil
}

// MustToUnstructured converts a concrete K8s API type to a un unstructured object and panics if not successful
func MustToUnstructured(obj interface{}) *unstructured.Unstructured {
	uObj, err := ToUnstructured(obj)
	if err != nil {
		panic(err)
	}
	return uObj
}

// ListAPIResources discovers all API resources supported by the Kube API sererver
func ListAPIResources(disco discovery.DiscoveryInterface) ([]metav1.APIResource, error) {
	apiResources := make([]metav1.APIResource, 0)
	resList, err := disco.ServerResources()
	if err != nil {
		return nil, errors.WithStack(err)
	}
	for _, resGroup := range resList {
		for _, apiRes := range resGroup.APIResources {
			apiResources = append(apiResources, apiRes)
		}
	}
	return apiResources, nil
}

// GetLiveResource returns the corresponding live resource from a unstructured object
func GetLiveResource(dclient dynamic.Interface, obj *unstructured.Unstructured, apiResource *metav1.APIResource, namespace string) (*unstructured.Unstructured, error) {
	resourceName := obj.GetName()
	if resourceName == "" {
		return nil, fmt.Errorf("resource was supplied without a name")
	}
	reIf := dclient.Resource(apiResource, namespace)
	liveObj, err := reIf.Get(resourceName, metav1.GetOptions{})
	if err != nil {
		if apierr.IsNotFound(err) {
			log.Infof("No live counterpart to %s/%s/%s/%s in namespace: '%s'", apiResource.Group, apiResource.Version, apiResource.Name, resourceName, namespace)
			return nil, nil
		}
		return nil, errors.WithStack(err)
	}
	return liveObj, nil
}

func WatchResourcesWithLabel(ctx context.Context, config *rest.Config, namespace string, labelName string) (chan watch.Event, error) {
	log.Infof("Start watching for resources changes with label %s in cluster %s", labelName, config.Host)
	dynClientPool := dynamic.NewDynamicClientPool(config)
	disco, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	serverResources, err := disco.ServerResources()
	if err != nil {
		return nil, err
	}

	resources := make([]dynamic.ResourceInterface, 0)
	for _, apiResourcesList := range serverResources {
		for i := range apiResourcesList.APIResources {
			apiResource := apiResourcesList.APIResources[i]
			watchSupported := false
			for _, verb := range apiResource.Verbs {
				if verb == "watch" {
					watchSupported = true
					break
				}
			}
			if watchSupported {
				dclient, err := dynClientPool.ClientForGroupVersionKind(schema.FromAPIVersionAndKind(apiResourcesList.GroupVersion, apiResource.Kind))
				if err != nil {
					return nil, err
				}
				resources = append(resources, dclient.Resource(&apiResource, namespace))
			}
		}
	}
	ch := make(chan watch.Event)
	go func() {
		var wg sync.WaitGroup
		wg.Add(len(resources))
		for i := 0; i < len(resources); i++ {
			resource := resources[i]
			go func() {
				defer wg.Done()
				watch, err := resource.Watch(metav1.ListOptions{LabelSelector: labelName})
				go func() {
					select {
					case <-ctx.Done():
						watch.Stop()
					}
				}()
				if err == nil {
					for event := range watch.ResultChan() {
						ch <- event
					}
				}
			}()
		}
		wg.Wait()
		close(ch)
		log.Infof("Stop watching for resources changes with label %s in cluster %s", labelName, config.ServerName)
	}()
	return ch, nil
}

// GetResourcesWithLabel returns all kubernetes resources with specified label
func GetResourcesWithLabel(config *rest.Config, namespace string, labelName string, labelValue string) ([]*unstructured.Unstructured, error) {
	dynClientPool := dynamic.NewDynamicClientPool(config)
	disco, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	resources, err := disco.ServerResources()
	if err != nil {
		return nil, err
	}

	var resourceInterfaces []dynamic.ResourceInterface

	for _, apiResourcesList := range resources {
		for i := range apiResourcesList.APIResources {
			apiResource := apiResourcesList.APIResources[i]
			listSupported := false
			for _, verb := range apiResource.Verbs {
				if verb == listVerb {
					listSupported = true
					break
				}
			}
			if listSupported {
				dclient, err := dynClientPool.ClientForGroupVersionKind(schema.FromAPIVersionAndKind(apiResourcesList.GroupVersion, apiResource.Kind))
				if err != nil {
					return nil, err
				}
				resourceInterfaces = append(resourceInterfaces, dclient.Resource(&apiResource, namespace))
			}
		}
	}

	var asyncErr error
	var result []*unstructured.Unstructured

	var wg sync.WaitGroup
	wg.Add(len(resourceInterfaces))
	for i := range resourceInterfaces {
		client := resourceInterfaces[i]
		go func() {
			defer wg.Done()
			list, err := client.List(metav1.ListOptions{
				LabelSelector: fmt.Sprintf("%s=%s", labelName, labelValue),
			})
			if err != nil {
				asyncErr = err
				return
			}
			// apply client side filtering since not every kubernetes API supports label filtering
			for i := range list.(*unstructured.UnstructuredList).Items {
				item := list.(*unstructured.UnstructuredList).Items[i]
				labels := item.GetLabels()
				if labels != nil {
					if value, ok := labels[labelName]; ok && value == labelValue {
						result = append(result, &item)
					}
				}
			}
		}()
	}
	wg.Wait()
	return result, asyncErr
}

// DeleteResourceWithLabel delete all resources which match to specified label selector
func DeleteResourceWithLabel(config *rest.Config, namespace string, labelName string, labelValue string) error {
	dynClientPool := dynamic.NewDynamicClientPool(config)
	disco, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return err
	}
	resources, err := disco.ServerResources()
	if err != nil {
		return err
	}

	var resourceInterfaces []struct {
		dynamic.ResourceInterface
		bool
	}

	for _, apiResourcesList := range resources {
		for i := range apiResourcesList.APIResources {
			apiResource := apiResourcesList.APIResources[i]
			deleteCollectionSupported := false
			deleteSupported := false
			for _, verb := range apiResource.Verbs {
				if verb == deleteCollectionVerb {
					deleteCollectionSupported = true
				} else if verb == deleteVerb {
					deleteSupported = true
				}
			}
			dclient, err := dynClientPool.ClientForGroupVersionKind(schema.FromAPIVersionAndKind(apiResourcesList.GroupVersion, apiResource.Kind))
			if err != nil {
				return err
			}

			if deleteCollectionSupported || deleteSupported {
				resourceInterfaces = append(resourceInterfaces, struct {
					dynamic.ResourceInterface
					bool
				}{dclient.Resource(&apiResource, namespace), deleteCollectionSupported})
			}
		}
	}

	var asyncErr error
	propagationPolicy := metav1.DeletePropagationForeground

	var wg sync.WaitGroup
	wg.Add(len(resourceInterfaces))

	for i := range resourceInterfaces {
		client := resourceInterfaces[i].ResourceInterface
		deleteCollectionSupported := resourceInterfaces[i].bool

		go func() {
			defer wg.Done()
			if deleteCollectionSupported {
				err = client.DeleteCollection(&metav1.DeleteOptions{
					PropagationPolicy: &propagationPolicy,
				}, metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", labelName, labelValue)})
				if err != nil && !apierr.IsNotFound(err) {
					asyncErr = err
				}
			} else {
				items, err := client.List(metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", labelName, labelValue)})
				if err != nil {
					asyncErr = err
					return
				}
				for _, item := range items.(*unstructured.UnstructuredList).Items {
					// apply client side filtering since not every kubernetes API supports label filtering
					labels := item.GetLabels()
					if labels != nil {
						if value, ok := labels[labelName]; ok && value == labelValue {
							err = client.Delete(item.GetName(), &metav1.DeleteOptions{
								PropagationPolicy: &propagationPolicy,
							})
							if err != nil && !apierr.IsNotFound(err) {
								asyncErr = err
								return
							}
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	return asyncErr
}

// GetLiveResources returns the corresponding live resource from a list of resources
func GetLiveResources(config *rest.Config, objs []*unstructured.Unstructured, namespace string) ([]*unstructured.Unstructured, error) {
	liveObjs := make([]*unstructured.Unstructured, len(objs))
	dynClientPool := dynamic.NewDynamicClientPool(config)
	disco, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	for i, obj := range objs {
		gvk := obj.GroupVersionKind()
		dclient, err := dynClientPool.ClientForGroupVersionKind(gvk)
		if err != nil {
			return nil, err
		}
		apiResource, err := ServerResourceForGroupVersionKind(disco, gvk)
		if err != nil {
			return nil, err
		}
		liveObj, err := GetLiveResource(dclient, obj, apiResource, namespace)
		if err != nil {
			return nil, err
		}
		liveObjs[i] = liveObj
	}
	return liveObjs, nil
}

// See: https://github.com/ksonnet/ksonnet/blob/master/utils/client.go
func ServerResourceForGroupVersionKind(disco discovery.DiscoveryInterface, gvk schema.GroupVersionKind) (*metav1.APIResource, error) {
	resources, err := disco.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		return nil, err
	}
	for _, r := range resources.APIResources {
		if r.Kind == gvk.Kind {
			log.Debugf("Chose API '%s' for %s", r.Name, gvk)
			return &r, nil
		}
	}
	return nil, fmt.Errorf("Server is unable to handle %s", gvk)
}

type listResult struct {
	Items []*unstructured.Unstructured `json:"items"`
}

// ListResources returns a list of resources of a particular API type using the dynamic client
func ListResources(dclient dynamic.Interface, apiResource metav1.APIResource, namespace string, listOpts metav1.ListOptions) ([]*unstructured.Unstructured, error) {
	reIf := dclient.Resource(&apiResource, namespace)
	liveObjs, err := reIf.List(listOpts)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	liveObjsBytes, err := json.Marshal(liveObjs)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	var objList listResult
	err = json.Unmarshal(liveObjsBytes, &objList)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return objList.Items, nil
}

// ListAllResources iterates the list of API resources, and returns all resources with the given filters
func ListAllResources(config *rest.Config, apiResources []metav1.APIResource, namespace string, listOpts metav1.ListOptions) ([]*unstructured.Unstructured, error) {
	// itemMap dedups items when there is duplication of a resource in multiple API types
	// e.g. extensions/v1beta1/namespaces/default/deployments and apps/v1/namespaces/default/deployments
	itemMap := make(map[string]*unstructured.Unstructured)

	for _, apiResource := range apiResources {
		dynConfig := *config
		dynConfig.GroupVersion = &schema.GroupVersion{
			Group:   apiResource.Group,
			Version: apiResource.Kind,
		}
		dclient, err := dynamic.NewClient(&dynConfig)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		resList, err := ListResources(dclient, apiResource, namespace, listOpts)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		for _, liveObj := range resList {
			itemMap[string(liveObj.GetUID())] = liveObj
		}

	}
	resources := make([]*unstructured.Unstructured, len(itemMap))
	i := 0
	for _, obj := range itemMap {
		resources[i] = obj
		i++
	}
	return resources, nil
}

// ApplyResource performs an apply of a unstructured resource
func ApplyResource(config *rest.Config, obj *unstructured.Unstructured, namespace string) (*unstructured.Unstructured, error) {
	log.Infof("Applying resource %s/%s in cluster: %s, namespace: %s", obj.GetKind(), obj.GetName(), config.Host, namespace)
	cmdArgs, err := formulateKubectlOptions(config)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	cmdArgs = append(cmdArgs, "-n", namespace, "apply", "-o", "json", "-f", "-")
	cmd := exec.Command("kubectl", cmdArgs...)
	cmd.Stdin = bytes.NewReader(manifestBytes)
	out, err := cmd.Output()
	if err != nil {
		exErr := err.(*exec.ExitError)
		return nil, fmt.Errorf("failed to apply '%s': %s", obj.GetName(), exErr.Stderr)
	}
	var liveObj unstructured.Unstructured
	err = json.Unmarshal(out, &liveObj)
	if err != nil {
		return nil, fmt.Errorf("failed to apply '%s': %s", obj.GetName(), err)
	}
	return &liveObj, nil
}

func writeTempFile(prefix string, data []byte) (string, error) {
	f, err := ioutil.TempFile(kubectlTempDir, prefix)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err != nil {
		return "", err
	}
	err = f.Close()
	if err != nil {
		return "", err
	}
	return f.Name(), nil
}

// formulateKubectlOptions returns a list of equivalent kubectl flags given a k8s rest.Config
func formulateKubectlOptions(config *rest.Config) ([]string, error) {
	opts := []string{
		"--server", config.Host,
	}
	if config.TLSClientConfig.Insecure {
		opts = append(opts, "--insecure-skip-tls-verify=true")
	}
	if config.TLSClientConfig.CAFile != "" {
		opts = append(opts, "--certificate-authority", config.TLSClientConfig.CAFile)
	} else if len(config.TLSClientConfig.CAData) > 0 {
		return nil, fmt.Errorf("Cannot generate kubectl options with cert-data")
	}
	if config.TLSClientConfig.CertFile != "" {
		opts = append(opts, "--client-certificate", config.TLSClientConfig.CertFile)
	} else if len(config.TLSClientConfig.CertData) > 0 {
		return nil, fmt.Errorf("Cannot generate kubectl options with cert-data")
	}
	if config.TLSClientConfig.KeyFile != "" {
		opts = append(opts, "--client-key", config.TLSClientConfig.KeyFile)
	} else if len(config.TLSClientConfig.KeyData) > 0 {
		return nil, fmt.Errorf("Cannot generate kubectl options with cert-data")
	}
	if config.Username != "" {
		opts = append(opts, "--username", config.Username)
	}
	if config.Password != "" {
		opts = append(opts, "--password", config.Password)
	}
	if config.BearerToken != "" {
		opts = append(opts, "--token", config.BearerToken)
	}
	return opts, nil
}

// GenerateTLSFiles examines the TLS settings of a rest.Config to see if it uses any TLS data
// (i.e. CAData, CertData, KeyData). It then creates them as temporary local files (which can
// later be used as arguments to a kubectl command), and updates the config with paths.
func GenerateTLSFiles(config *rest.Config) error {
	var host string
	if serverURL, err := url.Parse(config.Host); err != nil {
		host = serverURL.Host
	}
	if len(config.TLSClientConfig.CAData) > 0 && config.TLSClientConfig.CAFile == "" {
		fileName, err := writeTempFile(fmt.Sprintf("%s-ca.crt-", host), config.TLSClientConfig.CAData)
		if err != nil {
			return err
		}
		config.TLSClientConfig.CAFile = fileName
	}
	if len(config.TLSClientConfig.CertData) > 0 && config.TLSClientConfig.CertFile == "" {
		fileName, err := writeTempFile(fmt.Sprintf("%s-client.crt-", host), config.TLSClientConfig.CertData)
		if err != nil {
			return err
		}
		config.TLSClientConfig.CertFile = fileName
	}
	if len(config.TLSClientConfig.KeyData) > 0 && config.TLSClientConfig.KeyFile == "" {
		fileName, err := writeTempFile(fmt.Sprintf("%s-client.key-", host), config.TLSClientConfig.KeyData)
		if err != nil {
			return err
		}
		config.TLSClientConfig.KeyFile = fileName
	}
	return nil
}

func deleteFile(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return os.Remove(path)
}

// DeleteTLSFiles deletes any local TLS related files referenced by a rest Config.
func DeleteTLSFiles(config *rest.Config) error {
	var err error
	if config.TLSClientConfig.CAFile != "" {
		err = deleteFile(config.TLSClientConfig.CAFile)
		if err != nil {
			return err
		}
		config.TLSClientConfig.CAFile = ""
	}
	if config.TLSClientConfig.CertFile != "" {
		err = deleteFile(config.TLSClientConfig.CertFile)
		if err != nil {
			return err
		}
		config.TLSClientConfig.CertFile = ""
	}
	if config.TLSClientConfig.KeyFile != "" {
		err = deleteFile(config.TLSClientConfig.KeyFile)
		if err != nil {
			return err
		}
		config.TLSClientConfig.KeyFile = ""
	}
	return nil
}
