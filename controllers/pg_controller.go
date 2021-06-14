// Copyright (c) 2021 Aiven, Helsinki, Finland. https://aiven.io/

package controllers

import (
	"context"
	"fmt"

	"github.com/aiven/aiven-go-client"
	k8soperatorv1alpha1 "github.com/aiven/aiven-kubernetes-operator/api/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PGReconciler reconciles a PG object
type PGReconciler struct {
	Controller
}

// PGHandler handles an Aiven PG service
type PGHandler struct {
	Handlers
}

// +kubebuilder:rbac:groups=aiven.io,resources=pgs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=aiven.io,resources=pgs/status,verbs=get;update;patch

func (r *PGReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("pg", req.NamespacedName)
	log.Info("reconciling aiven pg")

	const pgServiceFinalizer = "pg-service-finalizer.aiven.io"
	pg := &k8soperatorv1alpha1.PG{}
	return r.reconcileInstance(&PGHandler{}, ctx, log, req, pg, pgServiceFinalizer)
}

func (r *PGReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&k8soperatorv1alpha1.PG{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

func (h PGHandler) exists(c *aiven.Client, log logr.Logger, i client.Object) (bool, error) {
	pg, err := h.convert(i)
	if err != nil {
		return false, err
	}

	log.Info("checking if pg service already exists")

	s, err := c.Services.Get(pg.Spec.Project, pg.Name)
	if aiven.IsNotFound(err) {
		return false, nil
	}

	return s != nil, nil
}

// create creates PG service and update CR status and creates secrets
func (h PGHandler) create(c *aiven.Client, log logr.Logger, i client.Object) (client.Object, error) {
	pg, err := h.convert(i)
	if err != nil {
		return nil, err
	}

	log.Info("creating a new pg service")

	var prVPCID *string
	if pg.Spec.ProjectVPCID != "" {
		prVPCID = &pg.Spec.ProjectVPCID
	}

	s, err := c.Services.Create(pg.Spec.Project, aiven.CreateServiceRequest{
		Cloud: pg.Spec.CloudName,
		MaintenanceWindow: getMaintenanceWindow(
			pg.Spec.MaintenanceWindowDow,
			pg.Spec.MaintenanceWindowTime),
		Plan:                pg.Spec.Plan,
		ProjectVPCID:        prVPCID,
		ServiceName:         pg.Name,
		ServiceType:         "pg",
		UserConfig:          UserConfigurationToAPI(pg.Spec.PGUserConfig).(map[string]interface{}),
		ServiceIntegrations: nil,
	})
	if err != nil {
		return nil, err
	}

	h.setStatus(pg, s)

	return pg, err
}

// update updates PG service and updates CR status
func (h PGHandler) update(c *aiven.Client, log logr.Logger, i client.Object) (client.Object, error) {
	pg, err := h.convert(i)
	if err != nil {
		return nil, err
	}

	log.Info("updating pg service")

	var prVPCID *string
	if pg.Spec.ProjectVPCID != "" {
		prVPCID = &pg.Spec.ProjectVPCID
	}

	s, err := c.Services.Update(pg.Spec.Project, pg.Name, aiven.UpdateServiceRequest{
		Cloud: pg.Spec.CloudName,
		MaintenanceWindow: getMaintenanceWindow(
			pg.Spec.MaintenanceWindowDow,
			pg.Spec.MaintenanceWindowTime),
		Plan:         pg.Spec.Plan,
		ProjectVPCID: prVPCID,
		UserConfig:   UserConfigurationToAPI(pg.Spec.PGUserConfig).(map[string]interface{}),
		Powered:      true,
	})
	if err != nil {
		return nil, err
	}

	h.setStatus(pg, s)

	return pg, nil
}

// setStatus updates PG CR status
func (h PGHandler) setStatus(pg *k8soperatorv1alpha1.PG, s *aiven.Service) {
	var prVPCID string

	if s.ProjectVPCID != nil {
		prVPCID = *s.ProjectVPCID
	}

	pg.Status.State = s.State
	pg.Status.ProjectVPCID = prVPCID
	pg.Status.Plan = s.Plan
	pg.Status.MaintenanceWindowTime = s.MaintenanceWindow.TimeOfDay
	pg.Status.MaintenanceWindowDow = s.MaintenanceWindow.DayOfWeek
	pg.Status.CloudName = s.CloudName
}

// delete deletes Aiven PG service
func (h PGHandler) delete(c *aiven.Client, log logr.Logger, i client.Object) (bool, error) {
	pg, err := h.convert(i)
	if err != nil {
		return false, err
	}

	// Delete PG on Aiven side
	if err := c.Services.Delete(pg.Spec.Project, pg.Name); err != nil {
		if !aiven.IsNotFound(err) {
			log.Error(err, "cannot delete aiven pg service")
			return false, fmt.Errorf("aiven client delete pg error: %w", err)
		}
	}

	log.Info("successfully finalized pg service on Aiven side")
	return true, nil
}

// getSecret retrieves a PG service secret
func (h PGHandler) getSecret(c *aiven.Client, log logr.Logger, i client.Object) (*corev1.Secret, error) {
	pg, err := h.convert(i)
	if err != nil {
		return nil, err
	}

	log.Info("getting pg secret")

	s, err := c.Services.Get(pg.Spec.Project, pg.Name)
	if err != nil {
		return nil, fmt.Errorf("cannot get pg: %w", err)
	}

	params := s.URIParams
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      h.getSecretName(pg),
			Namespace: pg.Namespace,
			Labels: map[string]string{
				"app": pg.Name,
			},
		},
		StringData: map[string]string{
			"PGHOST":       params["host"],
			"PGPORT":       params["port"],
			"PGDATABASE":   params["dbname"],
			"PGUSER":       params["user"],
			"PGPASSWORD":   params["password"],
			"PGSSLMODE":    params["sslmode"],
			"DATABASE_URI": s.URI,
		},
	}, nil
}

func (h PGHandler) getSecretName(pg *k8soperatorv1alpha1.PG) string {
	if pg.Spec.ConnInfoSecretTarget.Name != "" {
		return pg.Spec.ConnInfoSecretTarget.Name
	}
	return pg.Name
}

func (h PGHandler) isActive(c *aiven.Client, log logr.Logger, i client.Object) (bool, error) {
	pg, err := h.convert(i)
	if err != nil {
		return false, err
	}

	log.Info("checking if pg service is active")

	return checkServiceIsRunning(c, pg.Spec.Project, pg.Name), nil
}

func (h PGHandler) convert(i client.Object) (*k8soperatorv1alpha1.PG, error) {
	pg, ok := i.(*k8soperatorv1alpha1.PG)
	if !ok {
		return nil, fmt.Errorf("cannot convert object to PG")
	}

	return pg, nil
}

func (h PGHandler) checkPreconditions(*aiven.Client, logr.Logger, client.Object) bool {
	return true
}

func (h PGHandler) getSecretReference(i client.Object) *k8soperatorv1alpha1.AuthSecretReference {
	pg, err := h.convert(i)
	if err != nil {
		return nil
	}

	return &pg.Spec.AuthSecretRef
}
