package socialtest

import (
	"net/http"

	"github.com/grafana/grafana/pkg/login/social/connector"
	"github.com/grafana/grafana/pkg/login/social/models"
)

type FakeSocialService struct {
	ExpectedAuthInfoProvider *models.OAuthInfo
	ExpectedConnector        connector.SocialConnector
	ExpectedHttpClient       *http.Client
}

func (fss *FakeSocialService) GetOAuthProviders() map[string]bool {
	panic("not implemented")
}

func (fss *FakeSocialService) GetOAuthHttpClient(string) (*http.Client, error) {
	return fss.ExpectedHttpClient, nil
}

func (fss *FakeSocialService) GetConnector(string) (connector.SocialConnector, error) {
	return fss.ExpectedConnector, nil
}

func (fss *FakeSocialService) GetOAuthInfoProvider(string) *models.OAuthInfo {
	return fss.ExpectedAuthInfoProvider
}

func (fss *FakeSocialService) GetOAuthInfoProviders() map[string]*models.OAuthInfo {
	panic("not implemented")
}
