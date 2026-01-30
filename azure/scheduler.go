package azure

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/logic/armlogic"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

type Scheduler struct {
	ctx                context.Context
	credential         *azidentity.DefaultAzureCredential
	subscriptionId     string
	resourceGroup      string
	location           string
	cloudPrefix        string
	updateScheduleName string
	workflowsClient    *armlogic.WorkflowsClient
}

func NewScheduler(ctx context.Context, credential *azidentity.DefaultAzureCredential, subscriptionId, resourceGroup, location, cloudPrefix string) (*Scheduler, error) {
	workflowsClient, err := armlogic.NewWorkflowsClient(subscriptionId, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create workflows client: %w", err)
	}

	return &Scheduler{
		ctx:                ctx,
		credential:         credential,
		subscriptionId:     subscriptionId,
		resourceGroup:      resourceGroup,
		location:           location,
		cloudPrefix:        cloudPrefix,
		updateScheduleName: getScheduleName(cloudPrefix, common.UpdateCommand),
		workflowsClient:    workflowsClient,
	}, nil
}

func getScheduleName(prefix string, cmd common.Command) string {
	name := model.GetAgentProjectName(model.GetAgentPrefix(prefix), cmd)
	name = strings.ReplaceAll(name, "_", "-")
	if len(name) > 43 {
		name = name[:43]
	}
	return strings.TrimSuffix(name, "-")
}

func (s *Scheduler) getUpdateSchedule() (*armlogic.Workflow, error) {
	resp, err := s.workflowsClient.Get(s.ctx, s.resourceGroup, s.updateScheduleName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil, nil
		}
		return nil, err
	}
	return &resp.Workflow, nil
}

func (s *Scheduler) createUpdateSchedule(cron, agentJob, identity string) error {
	definition := s.getWorkflowDefinition(cron, agentJob)

	_, err := s.workflowsClient.CreateOrUpdate(s.ctx, s.resourceGroup, s.updateScheduleName,
		armlogic.Workflow{
			Location: to.Ptr(s.location),
			Properties: &armlogic.WorkflowProperties{
				State:      to.Ptr(armlogic.WorkflowStateEnabled),
				Definition: definition,
			},
			Tags: map[string]*string{
				model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("failed to create logic app: %w", err)
	}

	log.Printf("Created Logic App scheduler: %s\n", s.updateScheduleName)
	return nil
}

func (s *Scheduler) updateUpdateSchedule(cron, agentJob, identity string) error {
	definition := s.getWorkflowDefinition(cron, agentJob)

	_, err := s.workflowsClient.CreateOrUpdate(s.ctx, s.resourceGroup, s.updateScheduleName,
		armlogic.Workflow{
			Location: to.Ptr(s.location),
			Properties: &armlogic.WorkflowProperties{
				State:      to.Ptr(armlogic.WorkflowStateEnabled),
				Definition: definition,
			},
			Tags: map[string]*string{
				model.ResourceTagKey: to.Ptr(model.ResourceTagValue),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("failed to update logic app: %w", err)
	}

	log.Printf("Updated Logic App scheduler: %s\n", s.updateScheduleName)
	return nil
}

func (s *Scheduler) deleteUpdateSchedule() error {
	_, err := s.workflowsClient.Delete(s.ctx, s.resourceGroup, s.updateScheduleName, nil)
	if err != nil {
		var respErr *azcore.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == 404 {
			return nil
		}
		return err
	}
	log.Printf("Deleted Logic App scheduler: %s\n", s.updateScheduleName)
	return nil
}

func (s *Scheduler) getWorkflowDefinition(cron, agentJob string) map[string]interface{} {
	frequency, interval := parseCron(cron)

	containerAppJobId := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.App/jobs/%s",
		s.subscriptionId, s.resourceGroup, sanitizeJobName(agentJob))

	return map[string]interface{}{
		"$schema":        "https://schema.management.azure.com/providers/Microsoft.Logic/schemas/2016-06-01/workflowdefinition.json#",
		"contentVersion": "1.0.0.0",
		"triggers": map[string]interface{}{
			"Recurrence": map[string]interface{}{
				"type": "Recurrence",
				"recurrence": map[string]interface{}{
					"frequency": frequency,
					"interval":  interval,
				},
			},
		},
		"actions": map[string]interface{}{
			"Start_Container_App_Job": map[string]interface{}{
				"type": "Http",
				"inputs": map[string]interface{}{
					"method": "POST",
					"uri":    fmt.Sprintf("https://management.azure.com%s/start?api-version=2023-05-01", containerAppJobId),
					"authentication": map[string]interface{}{
						"type":     "ManagedServiceIdentity",
						"audience": "https://management.azure.com/",
					},
				},
			},
		},
	}
}

func parseCron(cron string) (string, int) {
	parts := strings.Fields(cron)
	if len(parts) < 5 {
		return "Day", 1
	}

	minute := parts[0]
	hour := parts[1]
	dayOfMonth := parts[2]
	_ = parts[3] // month - not used in Logic Apps
	dayOfWeek := parts[4]

	if dayOfMonth != "*" {
		return "Month", 1
	}

	if dayOfWeek != "*" {
		return "Week", 1
	}

	if hour != "*" {
		return "Day", 1
	}

	if minute != "*" {
		return "Hour", 1
	}

	return "Day", 1
}
