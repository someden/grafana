package ssosettingstests

import (
	context "context"

	"github.com/grafana/grafana/pkg/services/auth/identity"
	"github.com/grafana/grafana/pkg/services/ssosettings"
	models "github.com/grafana/grafana/pkg/services/ssosettings/models"
)

type ServiceFake struct {
	ExpectedList        []*models.SSOSetting
	ExpectedForProvider *models.SSOSetting
	ExpectedError       error
}

func (s *ServiceFake) List(ctx context.Context, requester identity.Requester) ([]*models.SSOSetting, error) {
	return s.ExpectedList, s.ExpectedError
}

func (s *ServiceFake) GetForProvider(ctx context.Context, provider string) (*models.SSOSetting, error) {
	return s.ExpectedForProvider, s.ExpectedError
}

func (s *ServiceFake) Upsert(ctx context.Context, provider string, data map[string]interface{}) error {
	return s.ExpectedError
}

func (s *ServiceFake) Delete(ctx context.Context, provider string) error {
	return s.ExpectedError
}

func (s *ServiceFake) Patch(ctx context.Context, provider string, data map[string]interface{}) error {
	return s.ExpectedError
}

func (s *ServiceFake) RegisterReloadable(ctx context.Context, provider string, reloadable ssosettings.Reloadable) {
}

func (s *ServiceFake) Reload(ctx context.Context, provider string) {}
