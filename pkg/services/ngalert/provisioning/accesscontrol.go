package provisioning

import (
	"context"

	ac "github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/auth/identity"
	"github.com/grafana/grafana/pkg/services/ngalert/accesscontrol"
	"github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
)

func NewRuleAccessControlService(ac *accesscontrol.RuleService) RuleAccessControlService {
	return &provisioningAccessControl{
		RuleService: ac,
	}
}

type provisioningAccessControl struct {
	*accesscontrol.RuleService
}

func (p *provisioningAccessControl) AuthorizeAccessToRuleGroup(ctx context.Context, user identity.Requester, rules models.RulesGroup) bool {
	if p.CanReadAllRules(ctx, user) {
		return true
	}
	return p.RuleService.AuthorizeAccessToRuleGroup(ctx, user, rules)
}

func (p *provisioningAccessControl) AuthorizeRuleChanges(ctx context.Context, user identity.Requester, change *store.GroupDelta) error {
	if p.CanWriteAllRules(ctx, user) {
		return nil
	}
	return p.RuleService.AuthorizeRuleChanges(ctx, user, change)
}

func (p *provisioningAccessControl) CanReadAllRules(ctx context.Context, user identity.Requester) bool {
	return p.HasAccess(ctx, user, ac.EvalAny(
		ac.EvalPermission(ac.ActionAlertingProvisioningRead),
		ac.EvalPermission(ac.ActionAlertingProvisioningReadSecrets),
	))
}

func (p *provisioningAccessControl) CanWriteAllRules(ctx context.Context, user identity.Requester) bool {
	return p.HasAccess(ctx, user, ac.EvalPermission(ac.ActionAlertingProvisioningWrite))
}
