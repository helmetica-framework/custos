package controllers

// The labels custos stamps on everything it owns. They live in their own file
// because the gather Job and the target Secret both carry them, and the two
// are built in different files.
const (
	arcanumNameLabel      = "custos.helmetica.io/arcanum-name"
	arcanumNamespaceLabel = "custos.helmetica.io/arcanum-namespace"
	arcanumUIDLabel       = "custos.helmetica.io/arcanum-uid"
)
