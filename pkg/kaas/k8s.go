package kaas

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"time"

	routeApi "github.com/openshift/api/route/v1"
	routeClient "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	k8s "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	deploymentRolloutTime = 5 * time.Minute
	deploymentLifetime    = 8 * time.Hour
	kasImage              = "kaas:static-kas"
	ciFetcherImage        = "registry.access.redhat.com/ubi8/ubi:8.5"
)

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

// TryLogin returns k8s clientset and route client
func TryLogin(kubeconfigPath string) (*k8s.Clientset, *routeClient.RouteV1Client, error) {
	config, err := buildConfig(kubeconfigPath)
	if err != nil {
		return nil, nil, err
	}

	// Seed random
	rand.Seed(time.Now().Unix())

	// creates the clientset
	k8sClient, err := k8s.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	// create route client
	routeClient, err := routeClient.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	return k8sClient, routeClient, err

}

func (s *ServerSettings) launchKASApp(appLabel string, tarBall string) (string, string, error) {
	replicas := int32(1)
	sharePIDNamespace := true
	ctx := context.TODO()
	createOpts := metav1.CreateOptions{}

	// Create service and route and fetch the host
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: appLabel,
			Labels: map[string]string{
				"app": appLabel,
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Port:     8080,
					Protocol: corev1.ProtocolTCP,
					Name:     "api",
				},
				{
					Port:     9000,
					Protocol: corev1.ProtocolTCP,
					Name:     "console",
				},
			},
			Selector: map[string]string{
				"app": appLabel,
			},
		},
	}
	_, err := s.K8sClient.CoreV1().Services(s.Namespace).Create(ctx, service, createOpts)
	if err != nil {
		return "", "", fmt.Errorf("failed to create new service: %s", err.Error())
	}

	kasAPIRoute := &routeApi.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-api", appLabel),
			Labels: map[string]string{
				"app": appLabel,
			},
		},
		Spec: routeApi.RouteSpec{
			To: routeApi.RouteTargetReference{
				Kind: "Service",
				Name: appLabel,
			},
			Port: &routeApi.RoutePort{
				TargetPort: intstr.FromInt(8080),
			},
			TLS: &routeApi.TLSConfig{
				Termination:                   routeApi.TLSTerminationEdge,
				InsecureEdgeTerminationPolicy: routeApi.InsecureEdgeTerminationPolicyRedirect,
			},
		},
	}
	apiRoute, err := s.RouteClient.Routes(s.Namespace).Create(ctx, kasAPIRoute, createOpts)
	if err != nil {
		return "", "", fmt.Errorf("failed to create route: %v", err)
	}
	externalAPIURL := fmt.Sprintf("https://%s", apiRoute.Spec.Host)
	consoleAPIRoute := &routeApi.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-console", appLabel),
			Labels: map[string]string{
				"app": appLabel,
			},
		},
		Spec: routeApi.RouteSpec{
			Path: "/",
			To: routeApi.RouteTargetReference{
				Kind: "Service",
				Name: appLabel,
			},
			Port: &routeApi.RoutePort{
				TargetPort: intstr.FromInt(9000),
			},
			TLS: &routeApi.TLSConfig{
				Termination:                   routeApi.TLSTerminationEdge,
				InsecureEdgeTerminationPolicy: routeApi.InsecureEdgeTerminationPolicyRedirect,
			},
		},
	}
	consoleRoute, err := s.RouteClient.Routes(s.Namespace).Create(ctx, consoleAPIRoute, createOpts)
	if err != nil {
		return "", "", fmt.Errorf("failed to create route: %v", err)
	}
	consoleURL := fmt.Sprintf("https://%s", consoleRoute.Spec.Host)

	// Declare and create new deployment
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-kas", appLabel),
			Labels: map[string]string{
				"app": appLabel,
			},
			Annotations: map[string]string{
				"alpha.image.policy.openshift.io/resolve-names": "*",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": appLabel,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": appLabel,
					},
				},
				Spec: corev1.PodSpec{
					InitContainers: []corev1.Container{
						{
							Name:  "ci-fetcher",
							Image: ciFetcherImage,
							Command: []string{
								"/bin/bash",
								"-c",
								`set -uxo pipefail && \
                                 umask 0000 && \
                                 curl -sL ${DUMPTAR} | tar xvz -m --no-overwrite-dir --checkpoint=.100 && \
                                 mv */* .`,
							},
							WorkingDir: "/must-gather/",
							Env: []corev1.EnvVar{
								{
									Name:  "DUMPTAR",
									Value: tarBall,
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "must-gather-volume",
									MountPath: "/must-gather/",
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "kas",
							Image: kasImage,
							Ports: []corev1.ContainerPort{
								{
									Name:          "ui",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: 8080,
								},
							},
							Args: []string{
								"--base-dir",
								"/must-gather/",
								"--kubeconfig",
								"/must-gather/kubeconfig",
							},
							ReadinessProbe: &corev1.Probe{
								TimeoutSeconds:   1,
								PeriodSeconds:    10,
								SuccessThreshold: 1,
								FailureThreshold: 3,
								Handler: corev1.Handler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/version",
										Port:   intstr.FromInt(8080),
										Scheme: "HTTP",
									},
								},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									"cpu":    resource.MustParse("100m"),
									"memory": resource.MustParse("500Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "must-gather-volume",
									MountPath: "/must-gather/",
								},
							},
						}, {
							Name:  "console",
							Image: "quay.io/openshift/origin-console:latest",
							Ports: []corev1.ContainerPort{
								{
									Name:          "ui",
									Protocol:      corev1.ProtocolTCP,
									ContainerPort: 9000,
								},
							},
							Args: []string{
								"/opt/bridge/bin/bridge",
								"--public-dir=/opt/bridge/static",
								"--k8s-mode=off-cluster",
								fmt.Sprintf("--k8s-mode-off-cluster-endpoint=%s", externalAPIURL),
								"--user-auth=disabled",
								"--k8s-auth=bearer-token",
								"--k8s-auth-bearer-token=dummy",
								"--user-settings-location=localstorage",
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									"cpu":    resource.MustParse("100m"),
									"memory": resource.MustParse("500Mi"),
								},
							},
						},
					},
					ShareProcessNamespace: &sharePIDNamespace,
					Volumes: []corev1.Volume{
						{
							Name: "must-gather-volume",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			},
		},
	}
	_, err = s.K8sClient.AppsV1().Deployments(s.Namespace).Create(ctx, deployment, createOpts)
	if err != nil {
		return "", "", fmt.Errorf("failed to create new deployment: %s", err.Error())
	}

	return externalAPIURL, consoleURL, nil
}

func (s *ServerSettings) waitForDeploymentReady(appLabel string) error {
	depName := fmt.Sprintf("%s-kas", appLabel)

	return wait.PollImmediate(time.Second, deploymentRolloutTime, func() (bool, error) {
		dep, err := s.K8sClient.AppsV1().Deployments(s.Namespace).Get(context.TODO(), depName, metav1.GetOptions{})
		if err != nil {
			return true, fmt.Errorf("failed to fetch deployment: %v", err)
		}
		return dep.Status.AvailableReplicas == 1, nil
	})
}

func (s *ServerSettings) deletePods(appLabel string) (string, error) {
	actionLog := []string{}
	ctx := context.TODO()
	delOptions := metav1.DeleteOptions{}

	// Delete service
	listOpts := metav1.ListOptions{LabelSelector: fmt.Sprintf("app=%s", appLabel)}
	svcList, err := s.K8sClient.CoreV1().Services(s.Namespace).List(ctx, listOpts)
	if err != nil || svcList.Items == nil {
		return "", fmt.Errorf("failed to find services: %v", err)
	}
	for _, svc := range svcList.Items {
		err := s.K8sClient.CoreV1().Services(s.Namespace).Delete(ctx, svc.Name, delOptions)
		if err != nil {
			return strings.Join(actionLog, "\n"),
				fmt.Errorf("error removing service %s: %v", svc.Name, err)
		}
		actionLog = append(actionLog, fmt.Sprintf("Removed service %s", svc.Name))
	}

	// Delete deployment
	depList, err := s.K8sClient.AppsV1().Deployments(s.Namespace).List(ctx, listOpts)
	if err != nil || depList.Items == nil {
		return "", fmt.Errorf("failed to find deployments: %v", err)
	}
	for _, dep := range depList.Items {
		err := s.K8sClient.AppsV1().Deployments(s.Namespace).Delete(ctx, dep.Name, delOptions)
		if err != nil {
			return strings.Join(actionLog, "\n"),
				fmt.Errorf("error removing deployment %s: %v", dep.Name, err)
		}
		actionLog = append(actionLog, fmt.Sprintf("Removed deployment %s", dep.Name))
	}

	// Delete configmap
	cmList, err := s.K8sClient.CoreV1().ConfigMaps(s.Namespace).List(ctx, listOpts)
	if err != nil || cmList.Items == nil {
		return "", fmt.Errorf("failed to find config maps: %v", err)
	}
	for _, cm := range cmList.Items {
		err := s.K8sClient.CoreV1().ConfigMaps(s.Namespace).Delete(ctx, cm.Name, delOptions)
		if err != nil {
			return strings.Join(actionLog, "\n"),
				fmt.Errorf("error removing config map %s: %v", cm.Name, err)
		}
		actionLog = append(actionLog, fmt.Sprintf("Removed config map %s", cm.Name))
	}

	// Delete route
	routeList, err := s.RouteClient.Routes(s.Namespace).List(ctx, listOpts)
	if err != nil || routeList.Items == nil {
		return "", fmt.Errorf("failed to find routes: %v", err)
	}
	for _, route := range routeList.Items {
		err := s.RouteClient.Routes(s.Namespace).Delete(ctx, route.Name, delOptions)
		if err != nil {
			return strings.Join(actionLog, "\n"),
				fmt.Errorf("error removing route %s: %v", route.Name, err)
		}
		actionLog = append(actionLog, fmt.Sprintf("Removed route %s", route.Name))
	}

	return strings.Join(actionLog, "\n"), nil
}

// CleanupOldDeployements periodically removes old deployments
func (s *ServerSettings) CleanupOldDeployements() {
	log.Println("Cleaning up old deployments")
	// List all deployments, find those which are older than n hours and call 'deletePods'
	depsList, err := s.K8sClient.AppsV1().Deployments(s.Namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil || depsList.Items == nil {
		return
	}
	now := time.Now()
	for _, dep := range depsList.Items {
		log.Printf("Found %s", dep.Name)
		// Get dep label and create time
		appLabel, ok := dep.Labels["app"]
		if !ok {
			log.Println("Deployment has no appLabel, skipping")
			// Deployment has no app label
			continue
		}
		createdAt := dep.GetCreationTimestamp()
		if now.After(createdAt.Add(deploymentLifetime)) {
			log.Println("Deployment will be garbage collected")
			go s.deletePods(appLabel)
		} else {
			log.Println("Deployment will live see another dawn")
		}
	}
}

// GetResourceQuota updates current resource quota setting
func (s *ServerSettings) GetResourceQuota() error {
	rquota, err := s.K8sClient.CoreV1().ResourceQuotas(s.Namespace).Get(context.TODO(), s.RQuotaName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ResourceQuota: %v", err)
	}
	s.RQStatus = &RQuotaStatus{
		Used: rquota.Status.Used.Pods().Value(),
		Hard: rquota.Status.Hard.Pods().Value(),
	}
	s.sendResourceQuotaUpdate()
	return nil
}

// WatchResourceQuota passes RQ updates from k8s to UI
func (s *ServerSettings) WatchResourceQuota() {
	for {
		// TODO: Make sure we watch correct resourceQuota
		watcher, err := s.K8sClient.CoreV1().ResourceQuotas(s.Namespace).Watch(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Printf("Failed to setup ResourceQuota watcher: %v", err)
			continue
		}
		ch := watcher.ResultChan()
		for event := range ch {
			rq, ok := event.Object.(*corev1.ResourceQuota)
			if !ok || rq.Name != s.RQuotaName {
				log.Printf("Skipping rq update: %v, %s", ok, rq.Name)
				continue
			}
			s.RQStatus = &RQuotaStatus{
				Used: rq.Status.Used.Pods().Value(),
				Hard: rq.Status.Hard.Pods().Value(),
			}
			log.Printf("ResourceQuota update: %v", s.RQStatus)
			s.sendResourceQuotaUpdate()
		}
	}
}
