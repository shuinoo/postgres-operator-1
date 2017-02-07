package cluster

import (
	"k8s.io/client-go/pkg/api/resource"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/apps/v1beta1"
	"k8s.io/client-go/pkg/util/intstr"

	"github.bus.zalan.do/acid/postgres-operator/pkg/util/k8sutil"
)

func (c *Cluster) createStatefulSet() {
	clusterName := (*c.cluster).Metadata.Name

	envVars := []v1.EnvVar{
		{
			Name:  "SCOPE",
			Value: clusterName,
		},
		{
			Name:  "PGROOT",
			Value: "/home/postgres/pgdata/pgroot",
		},
		{
			Name:  "ETCD_HOST",
			Value: c.etcdHost,
		},
		{
			Name: "POD_IP",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "status.podIP",
				},
			},
		},
		{
			Name: "POD_NAMESPACE",
			ValueFrom: &v1.EnvVarSource{
				FieldRef: &v1.ObjectFieldSelector{
					APIVersion: "v1",
					FieldPath:  "metadata.namespace",
				},
			},
		},
		{
			Name: "PGPASSWORD_SUPERUSER",
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{
					LocalObjectReference: v1.LocalObjectReference{
						Name: c.credentialSecretName("superuser"),
					},
					Key: "password",
				},
			},
		},
		{
			Name: "PGPASSWORD_STANDBY",
			ValueFrom: &v1.EnvVarSource{
				SecretKeyRef: &v1.SecretKeySelector{
					LocalObjectReference: v1.LocalObjectReference{
						Name: c.credentialSecretName("replication"),
					},
					Key: "password",
				},
			},
		},
	}

	resourceList := v1.ResourceList{}

	if cpu := (*c.cluster).Spec.Resources.Cpu; cpu != "" {
		resourceList[v1.ResourceCPU] = resource.MustParse(cpu)
	}

	if memory := (*c.cluster).Spec.Resources.Memory; memory != "" {
		resourceList[v1.ResourceMemory] = resource.MustParse(memory)
	}

	container := v1.Container{
		Name:            clusterName,
		Image:           c.dockerImage,
		ImagePullPolicy: v1.PullAlways,
		Resources: v1.ResourceRequirements{
			Requests: resourceList,
		},
		Ports: []v1.ContainerPort{
			{
				ContainerPort: 8008,
				Protocol:      v1.ProtocolTCP,
			},
			{
				ContainerPort: 5432,
				Protocol:      v1.ProtocolTCP,
			},
		},
		VolumeMounts: []v1.VolumeMount{
			{
				Name:      "pgdata",
				MountPath: "/home/postgres/pgdata", //TODO: fetch from manifesto
			},
		},
		Env: envVars,
	}

	terminateGracePeriodSeconds := int64(30)

	podSpec := v1.PodSpec{
		TerminationGracePeriodSeconds: &terminateGracePeriodSeconds,
		Volumes: []v1.Volume{
			{
				Name:         "pgdata",
				VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}},
			},
		},
		Containers: []v1.Container{container},
	}

	template := v1.PodTemplateSpec{
		ObjectMeta: v1.ObjectMeta{
			Labels:      c.labels(),
			Annotations: map[string]string{"pod.alpha.kubernetes.io/initialized": "true"},
		},
		Spec: podSpec,
	}

	statefulSet := &v1beta1.StatefulSet{
		ObjectMeta: v1.ObjectMeta{
			Name:   clusterName,
			Labels: c.labels(),
		},
		Spec: v1beta1.StatefulSetSpec{
			Replicas:    &c.cluster.Spec.NumberOfInstances,
			ServiceName: clusterName,
			Template:    template,
		},
	}

	c.config.KubeClient.StatefulSets(c.config.Namespace).Create(statefulSet)
}

func (c *Cluster) applySecrets() {
	var err error
	for _, user := range c.pgUsers {
		secret := v1.Secret{
			ObjectMeta: v1.ObjectMeta{
				Name:   c.credentialSecretName(string(user.username)),
				Labels: c.labels(),
			},
			Type: v1.SecretTypeOpaque,
			Data: map[string][]byte{
				"username": user.username,
				"password": user.password,
			},
		}
		_, err = c.config.KubeClient.Secrets(c.config.Namespace).Create(&secret)
		if k8sutil.IsKubernetesResourceAlreadyExistError(err) {
			_, err = c.config.KubeClient.Secrets(c.config.Namespace).Update(&secret)
			if err != nil {
				c.logger.Errorf("Error while updating secret: %+v", err)
			} else {
				c.logger.Infof("Secret updated: %+v", secret)
			}
		} else {
			if err != nil {
				c.logger.Errorf("Error while creating secret: %+v", err)
			} else {
				c.logger.Infof("Secret created: %+v", secret)
			}
		}
	}

	//TODO: remove secrets of the deleted users
}

func (c *Cluster) createService() {
	clusterName := (*c.cluster).Metadata.Name

	_, err := c.config.KubeClient.Services(c.config.Namespace).Get(clusterName)
	if !k8sutil.ResourceNotFound(err) {
		c.logger.Infof("Service '%s' already exists", clusterName)
		return
	}

	service := v1.Service{
		ObjectMeta: v1.ObjectMeta{
			Name:   clusterName,
			Labels: c.labels(),
		},
		Spec: v1.ServiceSpec{
			Type:  v1.ServiceTypeClusterIP,
			Ports: []v1.ServicePort{{Port: 5432, TargetPort: intstr.IntOrString{IntVal: 5432}}},
		},
	}

	_, err = c.config.KubeClient.Services(c.config.Namespace).Create(&service)
	if err != nil {
		c.logger.Errorf("Error while creating service: %+v", err)
	} else {
		c.logger.Infof("Service created: %+v", service)
	}
}

func (c *Cluster) createEndPoint() {
	clusterName := (*c.cluster).Metadata.Name

	_, err := c.config.KubeClient.Endpoints(c.config.Namespace).Get(clusterName)
	if !k8sutil.ResourceNotFound(err) {
		c.logger.Infof("Endpoint '%s' already exists", clusterName)
		return
	}

	endPoint := v1.Endpoints{
		ObjectMeta: v1.ObjectMeta{
			Name:   clusterName,
			Labels: c.labels(),
		},
	}

	_, err = c.config.KubeClient.Endpoints(c.config.Namespace).Create(&endPoint)
	if err != nil {
		c.logger.Errorf("Error while creating endpoint: %+v", err)
	} else {
		c.logger.Infof("Endpoint created: %+v", endPoint)
	}
}
