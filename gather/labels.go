package gather

import "sigs.k8s.io/controller-runtime/pkg/client"

// FieldOwner is the manager name on every server-side apply custos makes,
// from the controller and from the Job alike. Two names would have the two
// processes fight over the same fields instead of sharing them.
const FieldOwner = client.FieldOwner("custos")

// The labels custos stamps on everything it owns. They live here rather than
// in controllers because the Job stamps the arcanum-name label on the
// gathered Secret and the controller's cache selector reads it, so the two
// processes have to agree on the string.
const (
	ArcanumNameLabel      = "custos.helmetica.io/arcanum-name"
	ArcanumNamespaceLabel = "custos.helmetica.io/arcanum-namespace"
	ArcanumUIDLabel       = "custos.helmetica.io/arcanum-uid"
)
