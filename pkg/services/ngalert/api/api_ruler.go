package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/grafana/grafana/pkg/api/apierrors"
	"github.com/grafana/grafana/pkg/api/response"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/auth/identity"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/folder"
	"github.com/grafana/grafana/pkg/services/ngalert/accesscontrol"
	apimodels "github.com/grafana/grafana/pkg/services/ngalert/api/tooling/definitions"
	"github.com/grafana/grafana/pkg/services/ngalert/eval"
	ngmodels "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/provisioning"
	"github.com/grafana/grafana/pkg/services/ngalert/store"
	"github.com/grafana/grafana/pkg/services/quota"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
)

type ConditionValidator interface {
	// Validate validates that the condition is correct. Returns nil if the condition is correct. Otherwise, error that describes the failure
	Validate(ctx eval.EvaluationContext, condition ngmodels.Condition) error
}

type RulerSrv struct {
	xactManager        provisioning.TransactionManager
	provenanceStore    provisioning.ProvisioningStore
	store              RuleStore
	QuotaService       quota.Service
	log                log.Logger
	cfg                *setting.UnifiedAlertingSettings
	conditionValidator ConditionValidator
	authz              RuleAccessControlService
}

var (
	errProvisionedResource = errors.New("request affects resources created via provisioning API")
)

// RouteDeleteAlertRules deletes all alert rules the user is authorized to access in the given namespace
// or, if non-empty, a specific group of rules in the namespace.
// Returns http.StatusUnauthorized if user does not have access to any of the rules that match the filter.
// Returns http.StatusBadRequest if all rules that match the filter and the user is authorized to delete are provisioned.
func (srv RulerSrv) RouteDeleteAlertRules(c *contextmodel.ReqContext, namespaceUID string, group string) response.Response {
	namespace, err := srv.store.GetNamespaceByUID(c.Req.Context(), namespaceUID, c.SignedInUser.GetOrgID(), c.SignedInUser)
	if err != nil {
		return toNamespaceErrorResponse(err)
	}

	userNamespace, id := c.SignedInUser.GetNamespacedID()
	var loggerCtx = []any{
		"userId",
		id,
		"userNamespace",
		userNamespace,
		"namespaceUid",
		namespace.UID,
	}
	if group != "" {
		loggerCtx = append(loggerCtx, "group", group)
	}
	logger := srv.log.New(loggerCtx...)

	provenances, err := srv.provenanceStore.GetProvenances(c.Req.Context(), c.SignedInUser.GetOrgID(), (&ngmodels.AlertRule{}).ResourceType())
	if err != nil {
		return ErrResp(http.StatusInternalServerError, err, "failed to fetch provenances of alert rules")
	}

	err = srv.xactManager.InTransaction(c.Req.Context(), func(ctx context.Context) error {
		deletionCandidates := map[ngmodels.AlertRuleGroupKey]ngmodels.RulesGroup{}
		if group != "" {
			key := ngmodels.AlertRuleGroupKey{
				OrgID:        c.SignedInUser.GetOrgID(),
				NamespaceUID: namespace.UID,
				RuleGroup:    group,
			}
			rules, err := srv.getAuthorizedRuleGroup(ctx, c, key)
			if err != nil {
				return err
			}
			deletionCandidates[key] = rules
		} else {
			var totalGroups int
			deletionCandidates, totalGroups, err = srv.searchAuthorizedAlertRules(ctx, c, []string{namespace.UID}, "", 0)
			if err != nil {
				return err
			}
			if totalGroups > 0 && len(deletionCandidates) == 0 {
				return fmt.Errorf("%w to delete any existing rules in the namespace", accesscontrol.ErrAuthorization)
			}
		}
		rulesToDelete := make([]string, 0)
		provisioned := false
		for groupKey, rules := range deletionCandidates {
			if containsProvisionedAlerts(provenances, rules) {
				logger.Debug("Alert group cannot be deleted because it is provisioned", "group", groupKey.RuleGroup)
				provisioned = true
				continue
			}
			uid := make([]string, 0, len(rules))
			for _, rule := range rules {
				uid = append(uid, rule.UID)
			}
			rulesToDelete = append(rulesToDelete, uid...)
		}
		if len(rulesToDelete) > 0 {
			err := srv.store.DeleteAlertRulesByUID(ctx, c.SignedInUser.GetOrgID(), rulesToDelete...)
			if err != nil {
				return err
			}
			logger.Info("Alert rules were deleted", "ruleUid", strings.Join(rulesToDelete, ","))
			return nil
		}
		// if none rules were deleted return an error.
		// Check whether provisioned check failed first because if it is true, then all rules that the user can access (actually read via GET API) are provisioned.
		if provisioned {
			return errProvisionedResource
		}
		logger.Info("No alert rules were deleted")
		return nil
	})

	if err != nil {
		if errors.Is(err, accesscontrol.ErrAuthorization) {
			return ErrResp(http.StatusUnauthorized, err, "failed to delete rule group")
		}
		if errors.Is(err, errProvisionedResource) {
			return ErrResp(http.StatusBadRequest, err, "failed to delete rule group")
		}
		return ErrResp(http.StatusInternalServerError, err, "failed to delete rule group")
	}
	return response.JSON(http.StatusAccepted, util.DynMap{"message": "rules deleted"})
}

// RouteGetNamespaceRulesConfig returns all rules in a specific folder that user has access to
func (srv RulerSrv) RouteGetNamespaceRulesConfig(c *contextmodel.ReqContext, namespaceUID string) response.Response {
	namespace, err := srv.store.GetNamespaceByUID(c.Req.Context(), namespaceUID, c.SignedInUser.GetOrgID(), c.SignedInUser)
	if err != nil {
		return toNamespaceErrorResponse(err)
	}

	ruleGroups, _, err := srv.searchAuthorizedAlertRules(c.Req.Context(), c, []string{namespace.UID}, "", 0)
	if err != nil {
		return errorToResponse(err)
	}
	provenanceRecords, err := srv.provenanceStore.GetProvenances(c.Req.Context(), c.SignedInUser.GetOrgID(), (&ngmodels.AlertRule{}).ResourceType())
	if err != nil {
		return ErrResp(http.StatusInternalServerError, err, "failed to get provenance for rule group")
	}

	result := apimodels.NamespaceConfigResponse{}

	for groupKey, rules := range ruleGroups {
		key := getNamespaceKey(namespace)
		result[key] = append(result[key], toGettableRuleGroupConfig(groupKey.RuleGroup, rules, namespace.ID, provenanceRecords))
	}

	return response.JSON(http.StatusAccepted, result)
}

// RouteGetRulesGroupConfig returns rules that belong to a specific group in a specific namespace (folder).
// If user does not have access to at least one of the rule in the group, returns status 401 Unauthorized
func (srv RulerSrv) RouteGetRulesGroupConfig(c *contextmodel.ReqContext, namespaceUID string, ruleGroup string) response.Response {
	namespace, err := srv.store.GetNamespaceByUID(c.Req.Context(), namespaceUID, c.SignedInUser.GetOrgID(), c.SignedInUser)
	if err != nil {
		return toNamespaceErrorResponse(err)
	}

	rules, err := srv.getAuthorizedRuleGroup(c.Req.Context(), c, ngmodels.AlertRuleGroupKey{
		OrgID:        c.SignedInUser.GetOrgID(),
		RuleGroup:    ruleGroup,
		NamespaceUID: namespace.UID,
	})
	if err != nil {
		return errorToResponse(err)
	}

	provenanceRecords, err := srv.provenanceStore.GetProvenances(c.Req.Context(), c.SignedInUser.GetOrgID(), (&ngmodels.AlertRule{}).ResourceType())
	if err != nil {
		return ErrResp(http.StatusInternalServerError, err, "failed to get group alert rules")
	}

	result := apimodels.RuleGroupConfigResponse{
		GettableRuleGroupConfig: toGettableRuleGroupConfig(ruleGroup, rules, namespace.ID, provenanceRecords),
	}
	return response.JSON(http.StatusAccepted, result)
}

// RouteGetRulesConfig returns all alert rules that are available to the current user
func (srv RulerSrv) RouteGetRulesConfig(c *contextmodel.ReqContext) response.Response {
	namespaceMap, err := srv.store.GetUserVisibleNamespaces(c.Req.Context(), c.SignedInUser.GetOrgID(), c.SignedInUser)
	if err != nil {
		return ErrResp(http.StatusInternalServerError, err, "failed to get namespaces visible to the user")
	}
	result := apimodels.NamespaceConfigResponse{}

	if len(namespaceMap) == 0 {
		srv.log.Debug("User has no access to any namespaces")
		return response.JSON(http.StatusOK, result)
	}

	namespaceUIDs := make([]string, len(namespaceMap))
	for k := range namespaceMap {
		namespaceUIDs = append(namespaceUIDs, k)
	}

	dashboardUID := c.Query("dashboard_uid")
	panelID, err := getPanelIDFromRequest(c.Req)
	if err != nil {
		return ErrResp(http.StatusBadRequest, err, "invalid panel_id")
	}
	if dashboardUID == "" && panelID != 0 {
		return ErrResp(http.StatusBadRequest, errors.New("panel_id must be set with dashboard_uid"), "")
	}

	configs, _, err := srv.searchAuthorizedAlertRules(c.Req.Context(), c, namespaceUIDs, dashboardUID, panelID)
	if err != nil {
		return errorToResponse(err)
	}
	provenanceRecords, err := srv.provenanceStore.GetProvenances(c.Req.Context(), c.SignedInUser.GetOrgID(), (&ngmodels.AlertRule{}).ResourceType())
	if err != nil {
		return ErrResp(http.StatusInternalServerError, err, "failed to get alert rules")
	}

	for groupKey, rules := range configs {
		folder, ok := namespaceMap[groupKey.NamespaceUID]
		if !ok {
			userNamespace, id := c.SignedInUser.GetNamespacedID()
			srv.log.Error("Namespace not visible to the user", "user", id, "userNamespace", userNamespace, "namespace", groupKey.NamespaceUID)
			continue
		}
		key := getNamespaceKey(folder)
		result[key] = append(result[key], toGettableRuleGroupConfig(groupKey.RuleGroup, rules, folder.ID, provenanceRecords))
	}
	return response.JSON(http.StatusOK, result)
}

func (srv RulerSrv) RoutePostNameRulesConfig(c *contextmodel.ReqContext, ruleGroupConfig apimodels.PostableRuleGroupConfig, namespaceUID string) response.Response {
	namespace, err := srv.store.GetNamespaceByUID(c.Req.Context(), namespaceUID, c.SignedInUser.GetOrgID(), c.SignedInUser)
	if err != nil {
		return toNamespaceErrorResponse(err)
	}

	rules, err := validateRuleGroup(&ruleGroupConfig, c.SignedInUser.GetOrgID(), namespace, srv.cfg)
	if err != nil {
		return ErrResp(http.StatusBadRequest, err, "")
	}

	groupKey := ngmodels.AlertRuleGroupKey{
		OrgID:        c.SignedInUser.GetOrgID(),
		NamespaceUID: namespace.UID,
		RuleGroup:    ruleGroupConfig.Name,
	}

	return srv.updateAlertRulesInGroup(c, groupKey, rules)
}

// updateAlertRulesInGroup calculates changes (rules to add,update,delete), verifies that the user is authorized to do the calculated changes and updates database.
// All operations are performed in a single transaction
func (srv RulerSrv) updateAlertRulesInGroup(c *contextmodel.ReqContext, groupKey ngmodels.AlertRuleGroupKey, rules []*ngmodels.AlertRuleWithOptionals) response.Response {
	var finalChanges *store.GroupDelta
	err := srv.xactManager.InTransaction(c.Req.Context(), func(tranCtx context.Context) error {
		userNamespace, id := c.SignedInUser.GetNamespacedID()
		logger := srv.log.New("namespace_uid", groupKey.NamespaceUID, "group",
			groupKey.RuleGroup, "org_id", groupKey.OrgID, "user_id", id, "userNamespace", userNamespace)
		groupChanges, err := store.CalculateChanges(tranCtx, srv.store, groupKey, rules)
		if err != nil {
			return err
		}

		if groupChanges.IsEmpty() {
			finalChanges = groupChanges
			logger.Info("No changes detected in the request. Do nothing")
			return nil
		}

		err = srv.authz.AuthorizeRuleChanges(c.Req.Context(), c.SignedInUser, groupChanges)
		if err != nil {
			return err
		}

		if err := validateQueries(c.Req.Context(), groupChanges, srv.conditionValidator, c.SignedInUser); err != nil {
			return err
		}

		if err := verifyProvisionedRulesNotAffected(c.Req.Context(), srv.provenanceStore, c.SignedInUser.GetOrgID(), groupChanges); err != nil {
			return err
		}

		finalChanges = store.UpdateCalculatedRuleFields(groupChanges)
		logger.Debug("Updating database with the authorized changes", "add", len(finalChanges.New), "update", len(finalChanges.New), "delete", len(finalChanges.Delete))

		// Delete first as this could prevent future unique constraint violations.
		if len(finalChanges.Delete) > 0 {
			UIDs := make([]string, 0, len(finalChanges.Delete))
			for _, rule := range finalChanges.Delete {
				UIDs = append(UIDs, rule.UID)
			}

			if err = srv.store.DeleteAlertRulesByUID(tranCtx, c.SignedInUser.GetOrgID(), UIDs...); err != nil {
				return fmt.Errorf("failed to delete rules: %w", err)
			}
		}

		if len(finalChanges.Update) > 0 {
			updates := make([]ngmodels.UpdateRule, 0, len(finalChanges.Update))
			for _, update := range finalChanges.Update {
				logger.Debug("Updating rule", "rule_uid", update.New.UID, "diff", update.Diff.String())
				updates = append(updates, ngmodels.UpdateRule{
					Existing: update.Existing,
					New:      *update.New,
				})
			}
			err = srv.store.UpdateAlertRules(tranCtx, updates)
			if err != nil {
				return fmt.Errorf("failed to update rules: %w", err)
			}
		}

		if len(finalChanges.New) > 0 {
			inserts := make([]ngmodels.AlertRule, 0, len(finalChanges.New))
			for _, rule := range finalChanges.New {
				inserts = append(inserts, *rule)
			}
			added, err := srv.store.InsertAlertRules(tranCtx, inserts)
			if err != nil {
				return fmt.Errorf("failed to add rules: %w", err)
			}
			if len(added) != len(finalChanges.New) {
				logger.Error("Cannot match inserted rules with final changes", "insertedCount", len(added), "changes", len(finalChanges.New))
			} else {
				for i, newRule := range finalChanges.New {
					newRule.ID = added[i].ID
					newRule.UID = added[i].UID
				}
			}
		}

		if len(finalChanges.New) > 0 {
			userID, _ := identity.UserIdentifier(c.SignedInUser.GetNamespacedID())
			limitReached, err := srv.QuotaService.CheckQuotaReached(tranCtx, ngmodels.QuotaTargetSrv, &quota.ScopeParameters{
				OrgID:  c.SignedInUser.GetOrgID(),
				UserID: userID,
			}) // alert rule is table name
			if err != nil {
				return fmt.Errorf("failed to get alert rules quota: %w", err)
			}
			if limitReached {
				return ngmodels.ErrQuotaReached
			}
		}
		return nil
	})

	if err != nil {
		if errors.Is(err, ngmodels.ErrAlertRuleNotFound) {
			return ErrResp(http.StatusNotFound, err, "failed to update rule group")
		} else if errors.Is(err, ngmodels.ErrAlertRuleFailedValidation) || errors.Is(err, errProvisionedResource) {
			return ErrResp(http.StatusBadRequest, err, "failed to update rule group")
		} else if errors.Is(err, ngmodels.ErrQuotaReached) {
			return ErrResp(http.StatusForbidden, err, "")
		} else if errors.Is(err, accesscontrol.ErrAuthorization) {
			return ErrResp(http.StatusUnauthorized, err, "")
		} else if errors.Is(err, store.ErrOptimisticLock) {
			return ErrResp(http.StatusConflict, err, "")
		}
		return ErrResp(http.StatusInternalServerError, err, "failed to update rule group")
	}
	return changesToResponse(finalChanges)
}

func changesToResponse(finalChanges *store.GroupDelta) response.Response {
	body := apimodels.UpdateRuleGroupResponse{
		Message: "rule group updated successfully",
		Created: make([]string, 0, len(finalChanges.New)),
		Updated: make([]string, 0, len(finalChanges.Update)),
		Deleted: make([]string, 0, len(finalChanges.Delete)),
	}
	if finalChanges.IsEmpty() {
		body.Message = "no changes detected in the rule group"
	} else {
		for _, r := range finalChanges.New {
			body.Created = append(body.Created, r.UID)
		}
		for _, r := range finalChanges.Update {
			body.Updated = append(body.Updated, r.Existing.UID)
		}
		for _, r := range finalChanges.Delete {
			body.Deleted = append(body.Deleted, r.UID)
		}
	}
	return response.JSON(http.StatusAccepted, body)
}

func toGettableRuleGroupConfig(groupName string, rules ngmodels.RulesGroup, namespaceID int64, provenanceRecords map[string]ngmodels.Provenance) apimodels.GettableRuleGroupConfig {
	rules.SortByGroupIndex()
	ruleNodes := make([]apimodels.GettableExtendedRuleNode, 0, len(rules))
	var interval time.Duration
	if len(rules) > 0 {
		interval = time.Duration(rules[0].IntervalSeconds) * time.Second
	}
	for _, r := range rules {
		ruleNodes = append(ruleNodes, toGettableExtendedRuleNode(*r, namespaceID, provenanceRecords))
	}
	return apimodels.GettableRuleGroupConfig{
		Name:     groupName,
		Interval: model.Duration(interval),
		Rules:    ruleNodes,
	}
}

func toGettableExtendedRuleNode(r ngmodels.AlertRule, namespaceID int64, provenanceRecords map[string]ngmodels.Provenance) apimodels.GettableExtendedRuleNode {
	provenance := ngmodels.ProvenanceNone
	if prov, exists := provenanceRecords[r.ResourceID()]; exists {
		provenance = prov
	}
	gettableExtendedRuleNode := apimodels.GettableExtendedRuleNode{
		GrafanaManagedAlert: &apimodels.GettableGrafanaRule{
			ID:              r.ID,
			OrgID:           r.OrgID,
			Title:           r.Title,
			Condition:       r.Condition,
			Data:            ApiAlertQueriesFromAlertQueries(r.Data),
			Updated:         r.Updated,
			IntervalSeconds: r.IntervalSeconds,
			Version:         r.Version,
			UID:             r.UID,
			NamespaceUID:    r.NamespaceUID,
			NamespaceID:     namespaceID,
			RuleGroup:       r.RuleGroup,
			NoDataState:     apimodels.NoDataState(r.NoDataState),
			ExecErrState:    apimodels.ExecutionErrorState(r.ExecErrState),
			Provenance:      apimodels.Provenance(provenance),
			IsPaused:        r.IsPaused,
		},
	}
	forDuration := model.Duration(r.For)
	gettableExtendedRuleNode.ApiRuleNode = &apimodels.ApiRuleNode{
		For:         &forDuration,
		Annotations: r.Annotations,
		Labels:      r.Labels,
	}
	return gettableExtendedRuleNode
}

func toNamespaceErrorResponse(err error) response.Response {
	if errors.Is(err, ngmodels.ErrCannotEditNamespace) {
		return ErrResp(http.StatusForbidden, err, err.Error())
	}
	if errors.Is(err, dashboards.ErrDashboardIdentifierNotSet) {
		return ErrResp(http.StatusBadRequest, err, err.Error())
	}
	return apierrors.ToFolderErrorResponse(err)
}

// verifyProvisionedRulesNotAffected check that neither of provisioned alerts are affected by changes.
// Returns errProvisionedResource if there is at least one rule in groups affected by changes that was provisioned.
func verifyProvisionedRulesNotAffected(ctx context.Context, provenanceStore provisioning.ProvisioningStore, orgID int64, ch *store.GroupDelta) error {
	provenances, err := provenanceStore.GetProvenances(ctx, orgID, (&ngmodels.AlertRule{}).ResourceType())
	if err != nil {
		return err
	}
	errorMsg := strings.Builder{}
	for group, alertRules := range ch.AffectedGroups {
		if !containsProvisionedAlerts(provenances, alertRules) {
			continue
		}
		if errorMsg.Len() > 0 {
			errorMsg.WriteRune(',')
		}
		errorMsg.WriteString(group.String())
	}
	if errorMsg.Len() == 0 {
		return nil
	}
	return fmt.Errorf("%w: alert rule group [%s]", errProvisionedResource, errorMsg.String())
}

func validateQueries(ctx context.Context, groupChanges *store.GroupDelta, validator ConditionValidator, user identity.Requester) error {
	if len(groupChanges.New) > 0 {
		for _, rule := range groupChanges.New {
			err := validator.Validate(eval.NewContext(ctx, user), rule.GetEvalCondition())
			if err != nil {
				return fmt.Errorf("%w '%s': %s", ngmodels.ErrAlertRuleFailedValidation, rule.Title, err.Error())
			}
		}
	}
	if len(groupChanges.Update) > 0 {
		for _, upd := range groupChanges.Update {
			err := validator.Validate(eval.NewContext(ctx, user), upd.New.GetEvalCondition())
			if err != nil {
				return fmt.Errorf("%w '%s' (UID: %s): %s", ngmodels.ErrAlertRuleFailedValidation, upd.New.Title, upd.New.UID, err.Error())
			}
		}
	}
	return nil
}

// getAuthorizedRuleByUid fetches all rules in group to which the specified rule belongs, and checks whether the user is authorized to access the group.
// A user is authorized to access a group of rules only when it has permission to query all data sources used by all rules in this group.
// Returns rule identified by provided UID or ErrAuthorization if user is not authorized to access the rule.
func (srv RulerSrv) getAuthorizedRuleByUid(ctx context.Context, c *contextmodel.ReqContext, ruleUID string) (ngmodels.AlertRule, error) {
	q := ngmodels.GetAlertRulesGroupByRuleUIDQuery{
		UID:   ruleUID,
		OrgID: c.SignedInUser.GetOrgID(),
	}
	var err error
	rules, err := srv.store.GetAlertRulesGroupByRuleUID(ctx, &q)
	if err != nil {
		return ngmodels.AlertRule{}, err
	}
	if !srv.authz.AuthorizeAccessToRuleGroup(ctx, c.SignedInUser, rules) {
		return ngmodels.AlertRule{}, fmt.Errorf("%w to access rules in this group", accesscontrol.ErrAuthorization)
	}
	for _, rule := range rules {
		if rule.UID == ruleUID {
			return *rule, nil
		}
	}
	return ngmodels.AlertRule{}, ngmodels.ErrAlertRuleNotFound
}

// getAuthorizedRuleGroup fetches rules that belong to the specified models.AlertRuleGroupKey and validate user's authorization.
// A user is authorized to access a group of rules only when it has permission to query all data sources used by all rules in this group.
// Returns models.RuleGroup if authorization passed or ErrAuthorization if user is not authorized to access the rule.
func (srv RulerSrv) getAuthorizedRuleGroup(ctx context.Context, c *contextmodel.ReqContext, ruleGroupKey ngmodels.AlertRuleGroupKey) (ngmodels.RulesGroup, error) {
	q := ngmodels.ListAlertRulesQuery{
		OrgID:         ruleGroupKey.OrgID,
		NamespaceUIDs: []string{ruleGroupKey.NamespaceUID},
		RuleGroup:     ruleGroupKey.RuleGroup,
	}
	rules, err := srv.store.ListAlertRules(ctx, &q)
	if err != nil {
		return nil, err
	}
	if !srv.authz.AuthorizeAccessToRuleGroup(ctx, c.SignedInUser, rules) {
		return nil, fmt.Errorf("%w to access rules in this group", accesscontrol.ErrAuthorization)
	}
	return rules, nil
}

// searchAuthorizedAlertRules fetches rules according to the filters, groups them by models.AlertRuleGroupKey and filters out groups that the current user is not authorized to access.
// A user is authorized to access a group of rules only when it has permission to query all data sources used by all rules in this group.
// Returns groups that user is authorized to access, and total count of groups returned by query
func (srv RulerSrv) searchAuthorizedAlertRules(ctx context.Context, c *contextmodel.ReqContext, folderUIDs []string, dashboardUID string, panelID int64) (map[ngmodels.AlertRuleGroupKey]ngmodels.RulesGroup, int, error) {
	query := ngmodels.ListAlertRulesQuery{
		OrgID:         c.SignedInUser.GetOrgID(),
		NamespaceUIDs: folderUIDs,
		DashboardUID:  dashboardUID,
		PanelID:       panelID,
	}
	rules, err := srv.store.ListAlertRules(ctx, &query)
	if err != nil {
		return nil, 0, err
	}

	byGroupKey := ngmodels.GroupByAlertRuleGroupKey(rules)
	totalGroups := len(byGroupKey)
	for groupKey, rulesGroup := range byGroupKey {
		if !srv.authz.AuthorizeAccessToRuleGroup(ctx, c.SignedInUser, rulesGroup) {
			delete(byGroupKey, groupKey)
		}
	}
	return byGroupKey, totalGroups, nil
}

func getNamespaceKey(namespace *folder.Folder) string {
	return path.Join(namespace.ParentUID, namespace.Title)
}
