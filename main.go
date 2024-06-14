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

	for i, node := range nodes {
		if i >= 1 {
			break
		}

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

		err = uncordonNode(clientset, node.Name)
		CheckIfError(err)
	}

}

func handleRollout(clientset *kubernetes.Clientset, node *corev1.Node) error {
	// TODO: handle rollout for a node
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

	randomName := uuid.New()

	// Define the pod spec with hostPath volume and container
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("pod-%s", randomName),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  fmt.Sprintf("container-%s", randomName),
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
	_, err = clientset.CoreV1().Pods("kube-system").Create(context.TODO(), pod, metav1.CreateOptions{})
	CheckIfError(err)

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
	cmd := exec.Command("kubectl", "drain", "--ignore-daemonsets", "--delete-emptydir-data", "--force", node.Name)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()

	return err
}

func uncordonNode(clientset *kubernetes.Clientset, nodeName string) error {
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

	_, err = nodesClient.Patch(context.TODO(), nodeName, types.JSONPatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return err
	}

	fmt.Printf("node/%s uncordoned\n", nodeName)

	return nil
}

// intend to use for pods managed by daemonsets
func deleteAndWaitForNewPodCreated(clientset *kubernetes.Clientset, pod *corev1.Pod) error {
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, time.Second, informers.WithNamespace(pod.Namespace))

	// Create a channel to stop the informer
	stopCh := make(chan struct{})
	defer close(stopCh)

	informer := factory.Core().V1().Pods().Informer()
	// Add event handlers to the informer
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			newPod := newObj.(*corev1.Pod)

			if newPod.Labels["k8s-app"] == pod.Labels["k8s-app"] && newPod.Name != pod.Name && newPod.Spec.NodeName == pod.Spec.NodeName && newPod.Status.Phase == corev1.PodRunning {
				stopCh <- struct{}{}
				fmt.Printf("New pod \"%s\" is created\n", newPod.Name)
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
	fmt.Printf("Waiting for pod \"%s\" to be deleted and recreated\n", pod.Name)

	// Run the informer
	<-stopCh

	return nil
}
