package registry

import "github.com/grafana/grafana/pkg/login/social/connector"

//go:generate mockery --name OAuthConnectorRegistry --structname MockOAuthConnectorRegistry --outpkg registrytest --filename oauth_connector_registry_mock.go --output ./registrytest/
type OAuthConnectorRegistry interface {
	// Register registers a new OAuthConnector
	Register(provider string, connector connector.SocialConnector)

	// Get gets a OAuthConnector by name
	Get(name string) (connector.SocialConnector, bool)

	// GetAll gets all registered OAuthConnectors
	GetAll() []connector.SocialConnector
}
