package controllers

import (
	custosv1 "github.com/helmetica-framework/custos/api/v1"
	"github.com/helmetica-framework/custos/gather"
)

// The labels custos stamps on everything it owns, and the owner name for
// every apply. They are defined in gather because the Job writes objects
// carrying them too, and named again here because most of the controller has
// no other reason to know that package.
const (
	credentialConfigNameLabel      = gather.CredentialConfigNameLabel
	credentialConfigNamespaceLabel = gather.CredentialConfigNamespaceLabel
	credentialConfigUIDLabel       = gather.CredentialConfigUIDLabel

	fieldOwner = gather.FieldOwner
)

// ownershipLabels says which CredentialConfig an object belongs to.
//
// The name is what the manager cache selects Secrets on, and the UID is what
// the finalizer checks before deleting anything, since a name and namespace
// can be reused by a later CredentialConfig but a UID cannot.
func ownershipLabels(credentialConfig *custosv1.CredentialConfig) map[string]string {
	return map[string]string{
		credentialConfigNameLabel:      credentialConfig.GetName(),
		credentialConfigNamespaceLabel: credentialConfig.GetNamespace(),
		credentialConfigUIDLabel:       string(credentialConfig.GetUID()),
	}
}
