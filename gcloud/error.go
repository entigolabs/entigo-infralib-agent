package gcloud

import (
	"errors"

	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func isNotFoundError(err error) bool {
	var apiError *apierror.APIError
	if errors.As(err, &apiError) && (apiError.HTTPCode() == 404 || apiError.GRPCStatus().Code() == codes.NotFound) {
		return true
	}
	return status.Code(err) == codes.NotFound
}

func isAlreadyExistsError(err error) bool {
	var apiError *apierror.APIError
	if errors.As(err, &apiError) && apiError.GRPCStatus().Code() == codes.AlreadyExists {
		return true
	}
	return status.Code(err) == codes.AlreadyExists
}
