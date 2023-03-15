package notifier

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	alertingNotify "github.com/grafana/alerting/notify"
	alertingTemplates "github.com/grafana/alerting/templates"

	amv2 "github.com/prometheus/alertmanager/api/v2/models"

	"github.com/grafana/grafana/pkg/infra/kvstore"
	"github.com/grafana/grafana/pkg/infra/log"
	apimodels "github.com/grafana/grafana/pkg/services/ngalert/api/tooling/definitions"
	"github.com/grafana/grafana/pkg/services/ngalert/metrics"
	ngmodels "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
	"github.com/grafana/grafana/pkg/services/notifications"
	"github.com/grafana/grafana/pkg/setting"
)

const (
	notificationLogFilename = "notifications"
	silencesFilename        = "silences"

	workingDir = "alerting"
	// maintenanceNotificationAndSilences how often should we flush and garbage collect notifications
	notificationLogMaintenanceInterval = 15 * time.Minute
)

// How long should we keep silences and notification entries on-disk after they've served their purpose.
var retentionNotificationsAndSilences = 5 * 24 * time.Hour
var silenceMaintenanceInterval = 15 * time.Minute

type AlertingStore interface {
	store.AlertingStore
	store.ImageStore
}

type Alertmanager struct {
	Base   *alertingNotify.GrafanaAlertmanager
	logger log.Logger

	Settings  *setting.Cfg
	Store     AlertingStore
	fileStore *FileStore

	decryptFn           alertingNotify.GetDecryptedValueFn
	orgID               int64
	notificationService notifications.Service
}

// maintenanceOptions represent the options for components that need maintenance on a frequency within the Alertmanager.
// It implements the alerting.MaintenanceOptions interface.
type maintenanceOptions struct {
	filepath             string
	retention            time.Duration
	maintenanceFrequency time.Duration
	maintenanceFunc      func(alertingNotify.State) (int64, error)
}

func (m maintenanceOptions) Filepath() string {
	return m.filepath
}

func (m maintenanceOptions) Retention() time.Duration {
	return m.retention
}

func (m maintenanceOptions) MaintenanceFrequency() time.Duration {
	return m.maintenanceFrequency
}

func (m maintenanceOptions) MaintenanceFunc(state alertingNotify.State) (int64, error) {
	return m.maintenanceFunc(state)
}

func newAlertmanager(ctx context.Context, orgID int64, cfg *setting.Cfg, store AlertingStore, kvStore kvstore.KVStore,
	peer alertingNotify.ClusterPeer, decryptFn alertingNotify.GetDecryptedValueFn, ns notifications.Service,
	m *metrics.Alertmanager) (*Alertmanager, error) {
	workingPath := filepath.Join(cfg.DataPath, workingDir, strconv.Itoa(int(orgID)))
	fileStore := NewFileStore(orgID, kvStore, workingPath)

	nflogFilepath, err := fileStore.FilepathFor(ctx, notificationLogFilename)
	if err != nil {
		return nil, err
	}
	silencesFilePath, err := fileStore.FilepathFor(ctx, silencesFilename)
	if err != nil {
		return nil, err
	}

	silencesOptions := maintenanceOptions{
		filepath:             silencesFilePath,
		retention:            retentionNotificationsAndSilences,
		maintenanceFrequency: silenceMaintenanceInterval,
		maintenanceFunc: func(state alertingNotify.State) (int64, error) {
			// Detached context here is to make sure that when the service is shut down the persist operation is executed.
			return fileStore.Persist(context.Background(), silencesFilename, state)
		},
	}

	nflogOptions := maintenanceOptions{
		filepath:             nflogFilepath,
		retention:            retentionNotificationsAndSilences,
		maintenanceFrequency: notificationLogMaintenanceInterval,
		maintenanceFunc: func(state alertingNotify.State) (int64, error) {
			// Detached context here is to make sure that when the service is shut down the persist operation is executed.
			return fileStore.Persist(context.Background(), notificationLogFilename, state)
		},
	}

	amcfg := &alertingNotify.GrafanaAlertmanagerConfig{
		WorkingDirectory:   workingDir,
		AlertStoreCallback: nil,
		PeerTimeout:        cfg.UnifiedAlerting.HAPeerTimeout,
		Silences:           silencesOptions,
		Nflog:              nflogOptions,
	}

	l := log.New("alertmanager", "org", orgID)
	gam, err := alertingNotify.NewGrafanaAlertmanager("orgID", orgID, amcfg, peer, l, alertingNotify.NewGrafanaAlertmanagerMetrics(m.Registerer))
	if err != nil {
		return nil, err
	}

	am := &Alertmanager{
		Base:                gam,
		Settings:            cfg,
		Store:               store,
		notificationService: ns,
		orgID:               orgID,
		decryptFn:           decryptFn,
		fileStore:           fileStore,
		logger:              l,
	}

	return am, nil
}

func (am *Alertmanager) Ready() bool {
	// We consider AM as ready only when the config has been
	// applied at least once successfully. Until then, some objects
	// can still be nil.
	return am.Base.Ready()
}

func (am *Alertmanager) StopAndWait() {
	am.Base.StopAndWait()
}

// SaveAndApplyDefaultConfig saves the default configuration to the database and applies it to the Alertmanager.
// It rolls back the save if we fail to apply the configuration.
func (am *Alertmanager) SaveAndApplyDefaultConfig(ctx context.Context) error {
	var outerErr error
	am.Base.WithLock(func() {
		cmd := &ngmodels.SaveAlertmanagerConfigurationCmd{
			AlertmanagerConfiguration: am.Settings.UnifiedAlerting.DefaultConfiguration,
			Default:                   true,
			ConfigurationVersion:      fmt.Sprintf("v%d", ngmodels.AlertConfigurationVersion),
			OrgID:                     am.orgID,
			LastApplied:               time.Now().UTC().Unix(),
		}

		cfg, err := Load([]byte(am.Settings.UnifiedAlerting.DefaultConfiguration))
		if err != nil {
			outerErr = err
			return
		}

		err = am.Store.SaveAlertmanagerConfigurationWithCallback(ctx, cmd, func() error {
			_, err := am.applyConfig(cfg, []byte(am.Settings.UnifiedAlerting.DefaultConfiguration))
			return err
		})
		if err != nil {
			outerErr = nil
			return
		}
	})

	return outerErr
}

// SaveAndApplyConfig saves the configuration the database and applies the configuration to the Alertmanager.
// It rollbacks the save if we fail to apply the configuration.
func (am *Alertmanager) SaveAndApplyConfig(ctx context.Context, cfg *apimodels.PostableUserConfig) error {
	rawConfig, err := json.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("failed to serialize to the Alertmanager configuration: %w", err)
	}

	var outerErr error
	am.Base.WithLock(func() {
		cmd := &ngmodels.SaveAlertmanagerConfigurationCmd{
			AlertmanagerConfiguration: string(rawConfig),
			ConfigurationVersion:      fmt.Sprintf("v%d", ngmodels.AlertConfigurationVersion),
			OrgID:                     am.orgID,
			LastApplied:               time.Now().UTC().Unix(),
		}

		err = am.Store.SaveAlertmanagerConfigurationWithCallback(ctx, cmd, func() error {
			_, err := am.applyConfig(cfg, rawConfig)
			return err
		})
		if err != nil {
			outerErr = err
			return
		}
	})

	return outerErr
}

// ApplyConfig applies the configuration to the Alertmanager.
func (am *Alertmanager) ApplyConfig(ctx context.Context, dbCfg *ngmodels.AlertConfiguration) error {
	var err error
	cfg, err := Load([]byte(dbCfg.AlertmanagerConfiguration))
	if err != nil {
		return fmt.Errorf("failed to parse Alertmanager config: %w", err)
	}

	var outerErr error
	am.Base.WithLock(func() {
		if err := am.applyAndMarkConfig(ctx, dbCfg.ConfigurationHash, cfg, nil); err != nil {
			outerErr = fmt.Errorf("unable to apply configuration: %w", err)
			return
		}
	})

	return outerErr
}

// applyConfig applies a new configuration by re-initializing all components using the configuration provided.
// It returns a boolean indicating whether the user config was changed and an error.
// It is not safe to call concurrently.
func (am *Alertmanager) applyConfig(cfg *apimodels.PostableUserConfig, rawConfig []byte) (bool, error) {
	// First, let's make sure this config is not already loaded
	var amConfigChanged bool
	if rawConfig == nil {
		enc, err := json.Marshal(cfg.AlertmanagerConfig)
		if err != nil {
			// In theory, this should never happen.
			return false, err
		}
		rawConfig = enc
	}

	currentConfig := am.Base.Config()
	if currentConfig != nil && currentConfig.Hash() != md5.Sum(rawConfig) {
		amConfigChanged = true
	}

	receiverConfigs, err := postableReceiverToGrafanaReceiverConfig(context.Background(), cfg.AlertmanagerConfig.Receivers, am.decryptFn)
	if err != nil {
		return false, err
	}

	if cfg.TemplateFiles == nil {
		cfg.TemplateFiles = map[string]string{}
	}
	cfg.TemplateFiles["__default__.tmpl"] = alertingTemplates.DefaultTemplateString

	// next, we need to make sure we persist the templates to disk.
	paths, templatesChanged, err := PersistTemplates(cfg, am.WorkingDirPath())
	if err != nil {
		return false, err
	}

	// If neither the configuration nor templates have changed, we've got nothing to do.
	if !amConfigChanged && !templatesChanged {
		am.logger.Debug("neither config nor template have changed, skipping configuration sync.")
		return false, nil
	}

	// With the templates persisted, create the template list using the paths.
	tmpl, err := am.Base.TemplateFromPaths(am.Settings.AppURL, paths...)
	if err != nil {
		return false, err
	}

	err = am.Base.ApplyConfig(AlertingConfiguration{
		RawAlertmanagerConfig: rawConfig,
		AlertmanagerConfig:    cfg.AlertmanagerConfig,
		AlertmanagerTemplates: tmpl,
		ReceiversConfig:       receiverConfigs,
		DecryptFn:             am.decryptFn,
		ImageStore:            newImageStore(am.Store),
		LoggerFactory:         LoggerFactory,
		Sender:                NewNotificationSender(am.notificationService),
		Version:               setting.BuildVersion,
		OrgID:                 am.orgID,
	})
	if err != nil {
		return false, err
	}

	return true, nil
}

// applyAndMarkConfig applies a configuration and marks it as applied if no errors occur.
func (am *Alertmanager) applyAndMarkConfig(ctx context.Context, hash string, cfg *apimodels.PostableUserConfig, rawConfig []byte) error {
	configChanged, err := am.applyConfig(cfg, rawConfig)
	if err != nil {
		return err
	}

	if configChanged {
		markConfigCmd := ngmodels.MarkConfigurationAsAppliedCmd{
			OrgID:             am.orgID,
			ConfigurationHash: hash,
		}
		return am.Store.MarkConfigurationAsApplied(ctx, &markConfigCmd)
	}

	return nil
}

func (am *Alertmanager) WorkingDirPath() string {
	return filepath.Join(am.Settings.DataPath, workingDir, strconv.Itoa(int(am.orgID)))
}

// PutAlerts receives the alerts and then sends them through the corresponding route based on whenever the alert has a receiver embedded or not
func (am *Alertmanager) PutAlerts(postableAlerts apimodels.PostableAlerts) error {
	alerts := make(alertingNotify.PostableAlerts, 0, len(postableAlerts.PostableAlerts))
	for _, pa := range postableAlerts.PostableAlerts {
		alerts = append(alerts, &alertingNotify.PostableAlert{
			Annotations: pa.Annotations,
			EndsAt:      pa.EndsAt,
			StartsAt:    pa.StartsAt,
			Alert:       pa.Alert,
		})
	}

	return am.Base.PutAlerts(alerts)
}

// AlertValidationError is the error capturing the validation errors
// faced on the alerts.
type AlertValidationError struct {
	Alerts []amv2.PostableAlert
	Errors []error // Errors[i] refers to Alerts[i].
}

func (e AlertValidationError) Error() string {
	errMsg := ""
	if len(e.Errors) != 0 {
		errMsg = e.Errors[0].Error()
		for _, e := range e.Errors[1:] {
			errMsg += ";" + e.Error()
		}
	}
	return errMsg
}

type nilLimits struct{}

func (n nilLimits) MaxNumberOfAggregationGroups() int { return 0 }

func postableReceiverToGrafanaReceiverConfig(ctx context.Context, receivers []*apimodels.PostableApiReceiver, decryptFn alertingNotify.GetDecryptedValueFn) ([]alertingNotify.GrafanaReceiverConfig, error) {
	result := make([]alertingNotify.GrafanaReceiverConfig, 0, len(receivers))
	for _, receiver := range receivers {
		grafanaReceivers := make([]*alertingNotify.GrafanaReceiver, 0, len(receiver.GrafanaManagedReceivers))
		for _, managedReceiver := range receiver.GrafanaManagedReceivers {
			grafanaReceivers = append(grafanaReceivers, &alertingNotify.GrafanaReceiver{
				UID:                   managedReceiver.UID,
				Name:                  managedReceiver.Name,
				Type:                  managedReceiver.Type,
				DisableResolveMessage: managedReceiver.DisableResolveMessage,
				Settings:              json.RawMessage(managedReceiver.Settings),
				SecureSettings:        managedReceiver.SecureSettings,
			})
		}

		apiReceiver := &alertingNotify.APIReceiver{
			ConfigReceiver: receiver.Receiver,
			GrafanaReceivers: alertingNotify.GrafanaReceivers{
				Receivers: grafanaReceivers,
			},
		}

		pasedCfg, err := alertingNotify.BuildReceiverConfiguration(ctx, apiReceiver, decryptFn)
		if err != nil {
			return nil, err
		}
		result = append(result, pasedCfg)
	}

	return result, nil
}
