// Copyright (c) 2022 Aiven, Helsinki, Finland. https://aiven.io/

package controllers

import (
	"context"
	"fmt"

	"github.com/aiven/aiven-go-client"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aiven/aiven-operator/api/v1alpha1"
)

// KafkaReconciler reconciles a Kafka object
type KafkaReconciler struct {
	Controller
}

// +kubebuilder:rbac:groups=aiven.io,resources=kafkas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=aiven.io,resources=kafkas/status,verbs=get;update;patch

func (r *KafkaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.reconcileInstance(ctx, req, newGenericServiceHandler(newKafkaAdapter), &v1alpha1.Kafka{})
}

func (r *KafkaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Kafka{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

func newKafkaAdapter(avn *aiven.Client, object client.Object) (serviceAdapter, error) {
	kafka, ok := object.(*v1alpha1.Kafka)
	if !ok {
		return nil, fmt.Errorf("object is not of type v1alpha1.Kafka")
	}
	return &kafkaAdapter{avn: avn, Kafka: kafka}, nil
}

// kafkaAdapter handles an Aiven Kafka service
type kafkaAdapter struct {
	avn *aiven.Client
	*v1alpha1.Kafka
}

func (a *kafkaAdapter) getObjectMeta() *metav1.ObjectMeta {
	return &a.ObjectMeta
}

func (a *kafkaAdapter) getServiceStatus() *v1alpha1.ServiceStatus {
	return &a.Status
}

func (a *kafkaAdapter) getServiceCommonSpec() *v1alpha1.ServiceCommonSpec {
	return &a.Spec.ServiceCommonSpec
}

func (a *kafkaAdapter) getUserConfig() any {
	return &a.Spec.UserConfig
}

func (a *kafkaAdapter) newSecret(s *aiven.Service) (*corev1.Secret, error) {
	name := a.Spec.ConnInfoSecretTarget.Name
	if name == "" {
		name = a.Name
	}

	var userName, password string
	if len(s.Users) > 0 {
		userName = s.Users[0].Username
		password = s.Users[0].Password
	}

	caCert, err := a.avn.CA.Get(a.getServiceCommonSpec().Project)
	if err != nil {
		return nil, fmt.Errorf("aiven client error %w", err)
	}

	stringData := map[string]string{
		"HOST":        s.URIParams["host"],
		"PORT":        s.URIParams["port"],
		"PASSWORD":    password,
		"USERNAME":    userName,
		"ACCESS_CERT": s.ConnectionInfo.KafkaAccessCert,
		"ACCESS_KEY":  s.ConnectionInfo.KafkaAccessKey,
		"CA_CERT":     caCert,
	}

	// Removes empties
	for k, v := range stringData {
		if v == "" {
			delete(stringData, k)
		}
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: a.Namespace},
		StringData: stringData,
	}, nil
}

func (a *kafkaAdapter) getServiceType() string {
	return "kafka"
}

func (a *kafkaAdapter) getDiskSpace() string {
	return a.Spec.DiskSpace
}
