package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func TestSchemeRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))

	assert.True(t, scheme.Recognizes(GroupVersion.WithKind("Arcanum")))
	assert.True(t, scheme.Recognizes(GroupVersion.WithKind("ArcanumList")))
	assert.Equal(t, "arcana.helmetica.io", GroupVersion.Group)
	assert.Equal(t, "v1", GroupVersion.Version)
}

func TestArcanumDeepCopy(t *testing.T) {
	orig := &Arcanum{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sample",
			Namespace:  "default",
			Generation: 3,
			Labels:     map[string]string{"key": "value"},
		},
		Spec: ArcanumSpec{
			Suspend: ptr.To(true),
		},
		Status: ArcanumStatus{
			Phase:              ArcanumPhaseReady,
			ObservedGeneration: 3,
		},
	}

	cp := orig.DeepCopy()
	require.Equal(t, orig, cp)

	cp.Labels["key"] = "mutated"
	assert.Equal(t, "value", orig.Labels["key"], "deepcopy must not share the labels map")

	*cp.Spec.Suspend = false
	assert.True(t, *orig.Spec.Suspend, "deepcopy must not share the suspend pointer")
}

func TestArcanumDeepCopy_ValueMapping(t *testing.T) {
	orig := &Arcanum{
		ObjectMeta: metav1.ObjectMeta{Name: "credentials", Namespace: "instance"},
		Spec: ArcanumSpec{
			Target: TargetSpec{Name: "postgres-poc-credentials"},
			Credentials: CredentialsSpec{
				ValueMapping: map[string]ValueSource{
					"HOST": {Source: SourceConst, Value: "postgres-poc-rw"},
					"USERNAME": {
						Source: SourceObjectRef,
						Kind:   "Secret",
						Name:   "{{.ClaimName}}-app",
						Key:    "username",
					},
					"DATABASE": {
						Source: SourceExec,
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"app": "postgres"},
						},
						Command: []string{"psql", "-tAc", "select current_database()"},
					},
				},
			},
		},
	}

	cp := orig.DeepCopy()
	require.Equal(t, orig, cp)

	// The mapping is a map of structs holding a pointer and a slice. Every one
	// of those is a chance for the generated deepcopy to share memory.
	cp.Spec.Credentials.ValueMapping["HOST"] = ValueSource{Source: SourceConst, Value: "mutated"}
	assert.Equal(t, "postgres-poc-rw", orig.Spec.Credentials.ValueMapping["HOST"].Value,
		"deepcopy must not share the valueMapping map")

	cp.Spec.Credentials.ValueMapping["DATABASE"].PodSelector.MatchLabels["app"] = "mutated"
	assert.Equal(t, "postgres", orig.Spec.Credentials.ValueMapping["DATABASE"].PodSelector.MatchLabels["app"],
		"deepcopy must not share the podSelector pointer")

	cp.Spec.Credentials.ValueMapping["DATABASE"].Command[0] = "mutated"
	assert.Equal(t, "psql", orig.Spec.Credentials.ValueMapping["DATABASE"].Command[0],
		"deepcopy must not share the command slice")
}

func TestArcanumStatus_CarriesGatherState(t *testing.T) {
	orig := &Arcanum{
		Status: ArcanumStatus{
			Phase:           ArcanumPhaseReady,
			GatheredHash:    "a1b2c3d4",
			GatherJobName:   "credentials-gather-a1b2c3d4",
			SecretName:      "postgres-poc-credentials",
			SecretNamespace: "tenant",
		},
	}

	cp := orig.DeepCopy()
	assert.Equal(t, orig.Status, cp.Status)
}

func TestArcanumListDeepCopy(t *testing.T) {
	orig := &ArcanumList{
		Items: []Arcanum{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "one"},
				Spec:       ArcanumSpec{Suspend: ptr.To(true)},
			},
		},
	}

	cp := orig.DeepCopy()
	require.Equal(t, orig, cp)

	*cp.Items[0].Spec.Suspend = false
	assert.True(t, *orig.Items[0].Spec.Suspend,
		"deepcopy must not share the items slice")
}
