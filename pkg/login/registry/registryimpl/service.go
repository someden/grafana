package registryimpl

import (
	"github.com/grafana/grafana/pkg/login/registry"
	"github.com/grafana/grafana/pkg/login/social/connector"
)

var _ registry.OAuthConnectorRegistry = (*OAuthConnectorRegistryImpl)(nil)

type OAuthConnectorRegistryImpl struct {
	connectors map[string]*connector.SocialConnector
}

func ProvideOAuthConnectorRegistry() *OAuthConnectorRegistryImpl {
	return &OAuthConnectorRegistryImpl{
		connectors: make(map[string]*connector.SocialConnector),
	}
}

// Register registers a new OAuthConnector
func (s *OAuthConnectorRegistryImpl) Register(provider string, connector *connector.SocialConnector) {
	s.connectors[provider] = connector
}

// Get gets a OAuthConnector by name
func (s *OAuthConnectorRegistryImpl) Get(provider string) (*connector.SocialConnector, bool) {
	connector, ok := s.connectors[provider]
	return connector, ok
}

// GetAll gets all registered OAuthConnectors
func (s *OAuthConnectorRegistryImpl) GetAll() []*connector.SocialConnector {
	connectors := make([]*connector.SocialConnector, 0, len(s.connectors))
	for _, connector := range s.connectors {
		connectors = append(connectors, connector)
	}
	return connectors
}
