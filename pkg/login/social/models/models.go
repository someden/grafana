package models

import (
	"fmt"

	"github.com/grafana/grafana/pkg/services/org"
)

type OAuthInfo struct {
	ApiUrl                  string            `mapstructure:"api_url"`
	AuthUrl                 string            `mapstructure:"auth_url"`
	AuthStyle               string            `mapstructure:"auth_style"`
	ClientId                string            `mapstructure:"client_id"`
	ClientSecret            string            `mapstructure:"client_secret"`
	EmailAttributeName      string            `mapstructure:"email_attribute_name"`
	EmailAttributePath      string            `mapstructure:"email_attribute_path"`
	EmptyScopes             bool              `mapstructure:"empty_scopes"`
	GroupsAttributePath     string            `mapstructure:"groups_attribute_path"`
	HostedDomain            string            `mapstructure:"hosted_domain"`
	Icon                    string            `mapstructure:"icon"`
	Name                    string            `mapstructure:"name"`
	RoleAttributePath       string            `mapstructure:"role_attribute_path"`
	TeamIdsAttributePath    string            `mapstructure:"team_ids_attribute_path"`
	TeamsUrl                string            `mapstructure:"teams_url"`
	TlsClientCa             string            `mapstructure:"tls_client_ca"`
	TlsClientCert           string            `mapstructure:"tls_client_cert"`
	TlsClientKey            string            `mapstructure:"tls_client_key"`
	TokenUrl                string            `mapstructure:"token_url"`
	AllowedDomains          []string          `mapstructure:"allowed_domains"`
	AllowedGroups           []string          `mapstructure:"allowed_groups"`
	Scopes                  []string          `mapstructure:"scopes"`
	AllowAssignGrafanaAdmin bool              `mapstructure:"allow_assign_grafana_admin"`
	AllowSignup             bool              `mapstructure:"allow_sign_up"`
	AutoLogin               bool              `mapstructure:"auto_login"`
	Enabled                 bool              `mapstructure:"enabled"`
	RoleAttributeStrict     bool              `mapstructure:"role_attribute_strict"`
	TlsSkipVerify           bool              `mapstructure:"tls_skip_verify_insecure"`
	UsePKCE                 bool              `mapstructure:"use_pkce"`
	UseRefreshToken         bool              `mapstructure:"use_refresh_token"`
	Extra                   map[string]string `mapstructure:",remain"`
}

type BasicUserInfo struct {
	Id             string
	Name           string
	Email          string
	Login          string
	Role           org.RoleType
	IsGrafanaAdmin *bool // nil will avoid overriding user's set server admin setting
	Groups         []string
}

func (b *BasicUserInfo) String() string {
	return fmt.Sprintf("Id: %s, Name: %s, Email: %s, Login: %s, Role: %s, Groups: %v",
		b.Id, b.Name, b.Email, b.Login, b.Role, b.Groups)
}
