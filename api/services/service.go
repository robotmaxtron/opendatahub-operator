package services

import (
	"github.com/opendatahub-io/opendatahub-operator/v2/api/common"
)
// +groupName=dscinitialization.opendatahub.io

// Service struct defines the basis for each OpenDataHub component configuration.
// +kubebuilder:object:generate=true
type Service struct {
	common.ManagementSpec `json:",inline"`
}
