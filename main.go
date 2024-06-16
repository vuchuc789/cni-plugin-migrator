package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const CILIUM_IP_PREFIX = "10.123."
const CALICO_IP_PREFIX = "172.22."

func main() {
	var (
		kubeconfig                                       *string
		rollback                                         *bool
		controlPlaneNodes, etcdNodes, workerNodes, nodes []corev1.Node
	)

	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}

	rollback = flag.Bool("rollback", false, "proceed rollback")

	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	CheckIfError(err)

	clientset, err := kubernetes.NewForConfig(config)
	CheckIfError(err)

	nodesClient := clientset.CoreV1().Nodes()

	list, err := nodesClient.List(context.TODO(), metav1.ListOptions{})
	CheckIfError(err)

	for _, node := range list.Items {
		if isControlPlaneNode(&node) {
			controlPlaneNodes = append(controlPlaneNodes, node)
		} else if isEtcdNode(&node) {
			etcdNodes = append(etcdNodes, node)
		} else {
			workerNodes = append(workerNodes, node)
		}
	}

	nodes = append(nodes, controlPlaneNodes...)
	nodes = append(nodes, etcdNodes...)
	nodes = append(nodes, workerNodes...)

	for _, node := range nodes {
		if _, labelExists := node.Labels["io.cilium.migration/cilium-default"]; (!*rollback && labelExists) || (*rollback && !labelExists) {
			continue
		}

		err = drainNode(&node)
		CheckIfError(err)

		if !*rollback {
			err = handleRollout(clientset, &node)
			CheckIfError(err)
		} else {
			err = handleRollback(clientset, &node)
			CheckIfError(err)
		}

		err = uncordonNode(clientset, &node)
		CheckIfError(err)
	}

}

func handleRollout(clientset *kubernetes.Clientset, node *corev1.Node) error {
	nodesClient := clientset.CoreV1().Nodes()

	patchData := []byte(`{"metadata":{"labels":{"io.cilium.migration/cilium-default":"true"}}}`)

	_, err := nodesClient.Patch(context.TODO(), node.Name, types.StrategicMergePatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		return err
	}

	pods, err := clientset.CoreV1().Pods("kube-system").List(context.TODO(), metav1.ListOptions{
		LabelSelector: "k8s-app=cilium",
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
	})
	if err != nil {
		return err
	}

	var wg sync.WaitGroup

	for _, pod := range pods.Items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deleteAndWaitForNewPodCreated(clientset, &pod)
		}()
	}

	wg.Wait()

	pods, err = clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
	})
	if err != nil {
		return err
	}

	for _, pod := range pods.Items {
		if strings.HasPrefix(pod.Status.PodIP, CALICO_IP_PREFIX) && pod.Status.Phase == corev1.PodRunning {
			wg.Add(1)
			go func() {
				defer wg.Done()
				deleteAndWaitForNewPodCreated(clientset, &pod)
			}()
		}
	}
	wg.Wait()

	return nil
}

func handleRollback(clientset *kubernetes.Clientset, node *corev1.Node) error {
	nodesClient := clientset.CoreV1().Nodes()

	patchData := []byte(`{"metadata":{"labels":{"io.cilium.migration/cilium-default":null}}}`)

	_, err := nodesClient.Patch(context.TODO(), node.Name, types.StrategicMergePatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		return err
	}

	pods, err := clientset.CoreV1().Pods("kube-system").List(context.TODO(), metav1.ListOptions{
		LabelSelector: "k8s-app=cilium",
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
	})
	if err != nil {
		return err
	}

	var wg sync.WaitGroup

	for _, pod := range pods.Items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deleteAndWaitForNewPodCreated(clientset, &pod)
		}()
	}

	wg.Wait()

	err = modifyCNIConfig(clientset, node)
	if err != nil {
		return err
	}

	pods, err = clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{
		FieldSelector: fmt.Sprintf("spec.nodeName=%s", node.Name),
	})
	if err != nil {
		return err
	}

	for _, pod := range pods.Items {
		if strings.HasPrefix(pod.Status.PodIP, CILIUM_IP_PREFIX) && pod.Status.Phase == corev1.PodRunning {
			wg.Add(1)
			go func() {
				defer wg.Done()
				deleteAndWaitForNewPodCreated(clientset, &pod)
			}()
		}
	}
	wg.Wait()

	return nil
}

// isControlPlaneNode checks if a node is a control plane node
func isControlPlaneNode(node *corev1.Node) bool {
	if _, ok := node.Labels["node-role.kubernetes.io/controlplane"]; ok {
		return true
	}

	if _, ok := node.Labels["node-role.kubernetes.io/master"]; ok {
		return true
	}

	if _, ok := node.Labels["node-role.kubernetes.io/control-plane"]; ok {
		return true
	}

	return false
}

// isEtcdNode checks if a node is an etcd node
func isEtcdNode(node *corev1.Node) bool {
	_, ok := node.Labels["node-role.kubernetes.io/etcd"]
	return ok
}

// isWorkerNode checks if a node is a worker node
func isWorkerNode(node *corev1.Node) bool {
	_, ok := node.Labels["node-role.kubernetes.io/worker"]
	return ok
}

func drainNode(node *corev1.Node) error {
	// leverage drain command instead of reinventing the wheel by using eviction api
	cmd := exec.Command("kubectl", "drain", "--ignore-daemonsets", "--delete-emptydir-data", "--force", node.Name)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()

	return err
}

func uncordonNode(clientset *kubernetes.Clientset, node *corev1.Node) error {
	nodesClient := clientset.CoreV1().Nodes()

	patchData := []map[string]interface{}{
		{
			"op":    "replace",
			"path":  "/spec/unschedulable",
			"value": false,
		},
	}
	patchBytes, err := json.Marshal(patchData)
	if err != nil {
		return err
	}

	_, err = nodesClient.Patch(context.TODO(), node.Name, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return err
	}

	fmt.Printf("node/%s uncordoned\n", node.Name)

	return nil
}

// intend to use for pods managed by daemonsets
func deleteAndWaitForNewPodCreated(clientset *kubernetes.Clientset, pod *corev1.Pod) error {
	splittedPodNamePrefix := strings.Split(pod.Name, "-")
	splittedPodNamePrefix = splittedPodNamePrefix[:len(splittedPodNamePrefix)-1]
	podNamePrefix := strings.Join(splittedPodNamePrefix, "-")

	factory := informers.NewSharedInformerFactoryWithOptions(clientset, time.Second, informers.WithNamespace(pod.Namespace))

	// Create a channel to stop the informer
	stopCh := make(chan struct{})
	var stopped bool

	informer := factory.Core().V1().Pods().Informer()
	// Add event handlers to the informer
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			newPod := newObj.(*corev1.Pod)

			newSplittedPodNamePrefix := strings.Split(newPod.Name, "-")
			newSplittedPodNamePrefix = newSplittedPodNamePrefix[:len(newSplittedPodNamePrefix)-1]
			newPodNamePrefix := strings.Join(newSplittedPodNamePrefix, "-")

			if newPodNamePrefix == podNamePrefix &&
				newPod.Name != pod.Name &&
				newPod.Spec.NodeName == pod.Spec.NodeName &&
				newPod.Status.Phase == corev1.PodRunning {

				if !stopped {
					stopped = true
					close(stopCh)
					fmt.Printf("pod/%s created\n", pod.Name)
				}
			}
		},
	})

	// Start the informer
	factory.Start(stopCh)
	// Wait for the cache to sync
	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		return errors.New("Failed to sync caches")
	}

	err := clientset.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	fmt.Printf("pod/%s deleted\n", pod.Name)

	// Run the informer
	<-stopCh

	return nil
}

func modifyCNIConfig(clientset *kubernetes.Clientset, node *corev1.Node) error {
	randomName := fmt.Sprintf("cni-config-modifier-%s", uuid.New())
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, time.Second, informers.WithNamespace("kube-system"))

	// Create a channel to stop the informer
	stopCh := make(chan struct{})
	var stopped bool

	informer := factory.Core().V1().Pods().Informer()
	// Add event handlers to the informer
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			newPod := newObj.(*corev1.Pod)

			if newPod.Name == randomName &&
				newPod.Status.Phase == corev1.PodSucceeded {

				if !stopped {
					stopped = true
					close(stopCh)
					fmt.Printf("pod/%s completed\n", randomName)
				}
			}
		},
	})

	// Start the informer
	factory.Start(stopCh)
	// Wait for the cache to sync
	if !cache.WaitForCacheSync(stopCh, informer.HasSynced) {
		return errors.New("Failed to sync caches")
	}

	// Define the pod spec with hostPath volume and container
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: randomName,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  randomName,
					Image: "busybox",
					Command: []string{
						"sh", "-c",
						"mv /etc/cni/net.d/05-cilium.conflist /etc/cni/net.d/05-cilium.conflist.bak && mv /etc/cni/net.d/10-calico.conflist.cilium_bak /etc/cni/net.d/10-calico.conflist",
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "cni-config",
							MountPath: "/etc/cni/net.d",
						},
					},
				},
			},
			// NodeSelector to schedule the pod on a specific node
			NodeSelector: map[string]string{
				"kubernetes.io/hostname": node.Name,
			},
			// Tolerations to ignore all taints
			Tolerations: []corev1.Toleration{
				{
					Operator: corev1.TolerationOpExists,
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "cni-config",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/etc/cni/net.d/",
						},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	// Create the pod
	_, err := clientset.CoreV1().Pods("kube-system").Create(context.TODO(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	fmt.Printf("pod/%s created\n", randomName)

	// Run the informer
	<-stopCh

	return nil
}
