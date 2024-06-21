package gcloud

import (
	"context"
	"testing"

	"github.com/entigolabs/entigo-infralib-agent/common"
)

func TestFunctionName(t *testing.T) {
	resourceManager, err := NewResourceManager(context.Background())
	if err != nil {
		common.Logger.Fatalf("%v", err)
	}

	projectID := "entigo-infralib"
	serviceAccountName := "il-test-sa"
	serviceAccountDesc := "Infra Library Test Service Account"

	_, err = resourceManager.GetOrCreateServiceAccount(projectID, serviceAccountName, serviceAccountDesc)
	if err != nil {
		common.Logger.Fatalf("%v", err)
	}

	SANameFullLen := "projects/" + projectID + "/serviceAccounts/" + serviceAccountName + "@" + projectID + ".iam.gserviceaccount.com"
	resourceManager.AddRolesToServiceAccount(
		SANameFullLen,
		[]string{"roles/editor", "roles/iam.securityAdmin", "roles/iam.serviceAccountAdmin"},
	)
	if err != nil {
		common.Logger.Fatalf("%v", err)
	}

	resourceManager.AddRolesToProject(
		SANameFullLen,
		[]string{"roles/editor", "roles/iam.securityAdmin", "roles/iam.serviceAccountAdmin", "roles/container.admin"},
	)
	if err != nil {
		common.Logger.Fatalf("%v", err)
	}

	common.Logger.Println("success!")
}