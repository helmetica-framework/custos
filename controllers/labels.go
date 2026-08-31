package controllers

import "github.com/helmetica-framework/custos/gather"

// The labels custos stamps on everything it owns, and the owner name for
// every apply. They are defined in gather because the Job writes objects
// carrying them too, and named again here because most of the controller has
// no other reason to know that package.
const (
	arcanumNameLabel      = gather.ArcanumNameLabel
	arcanumNamespaceLabel = gather.ArcanumNamespaceLabel
	arcanumUIDLabel       = gather.ArcanumUIDLabel

	fieldOwner = gather.FieldOwner
)
