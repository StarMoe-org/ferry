package fake

import (
	"context"
	"testing"

	"github.com/StarMoe-org/ferry/internal/kube"
)

func TestFakeServerAndKubeClient(t *testing.T) {
	srv := NewTLS()
	defer srv.Close()
	srv.SetToken("secret-tok")

	ctx := context.Background()
	cl, err := srv.Client("de")
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	v, err := cl.Version(ctx)
	if err != nil || v != "v1.34.1+k3s1" {
		t.Fatalf("Version = %q, %v", v, err)
	}

	// Wrong token should 401.
	badCfg := srv.Config("de")
	badCfg.Token = "wrong"
	badCl, err := kube.New(badCfg)
	if err != nil {
		t.Fatalf("kube.New: %v", err)
	}
	if _, err := badCl.Version(ctx); err == nil {
		t.Fatal("expected 401 with wrong token")
	}

	// Seed deployment and test JSON patch (test + replace + add change-cause).
	srv.Seed(map[string]any{
		"kind": "Deployment",
		"metadata": map[string]any{
			"name":      "web",
			"namespace": "app",
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": "web", "image": "reg/app:old"},
					},
				},
			},
		},
	})

	// Failed test op must 422 and leave object untouched.
	badPatch := []map[string]any{
		{"op": "test", "path": "/spec/template/spec/containers/0/image", "value": "reg/app:mismatch"},
		{"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "reg/app:new"},
	}
	err = cl.Patch(ctx, "/apis/apps/v1/namespaces/app/deployments/web?fieldManager=ferry", "application/json-patch+json", badPatch, nil)
	if !kube.IsUnprocessable(err) {
		t.Fatalf("expected 422 on failed test op, got %v", err)
	}
	obj, _ := srv.GetObject("deployments", "app", "web")
	gotImg := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["image"]
	if gotImg != "reg/app:old" {
		t.Fatalf("failed patch mutated object: %v", gotImg)
	}

	// Valid patch updates image, sets change-cause, and records fieldManager=ferry.
	goodPatch := []map[string]any{
		{"op": "test", "path": "/spec/template/spec/containers/0/image", "value": "reg/app:old"},
		{"op": "replace", "path": "/spec/template/spec/containers/0/image", "value": "reg/app:new"},
		{"op": "add", "path": "/metadata/annotations", "value": map[string]any{}},
		{"op": "add", "path": "/metadata/annotations/kubernetes.io~1change-cause", "value": "reg/app:new github:org/app@1234567"},
	}
	if err := cl.Patch(ctx, "/apis/apps/v1/namespaces/app/deployments/web?fieldManager=ferry", "application/json-patch+json", goodPatch, nil); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	patches := srv.Patches()
	if len(patches) != 1 || patches[0].FieldManager != "ferry" {
		t.Fatalf("unexpected recorded patches: %+v", patches)
	}

	// Merge-patch sets and removes annotation.
	if err := cl.Patch(ctx, "/apis/apps/v1/namespaces/app/deployments/web", "application/merge-patch+json", map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{"ferry.star.moe/auto-deploy": "false"},
		},
	}, nil); err != nil {
		t.Fatalf("merge patch set: %v", err)
	}
	if err := cl.Patch(ctx, "/apis/apps/v1/namespaces/app/deployments/web", "application/merge-patch+json", map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{"ferry.star.moe/auto-deploy": nil},
		},
	}, nil); err != nil {
		t.Fatalf("merge patch delete: %v", err)
	}
	obj, _ = srv.GetObject("deployments", "app", "web")
	ann := obj["metadata"].(map[string]any)["annotations"].(map[string]any)
	if _, exists := ann["ferry.star.moe/auto-deploy"]; exists {
		t.Fatalf("expected annotation removed, got %v", ann)
	}
	if ann["kubernetes.io/change-cause"] != "reg/app:new github:org/app@1234567" {
		t.Fatalf("expected change-cause preserved, got %v", ann)
	}

	// ConfigMap create + optimistic concurrency conflict.
	var created map[string]any
	if err := cl.Do(ctx, "POST", "/api/v1/namespaces/app/configmaps", map[string]any{
		"metadata": map[string]any{"name": "ferry-state"},
		"data":     map[string]any{"state.json": "{}"},
	}, "application/json", &created); err != nil {
		t.Fatalf("POST configmap: %v", err)
	}
	srv.InjectConflictOnce()
	err = cl.Do(ctx, "PUT", "/api/v1/namespaces/app/configmaps/ferry-state", created, "application/json", nil)
	if !kube.IsConflict(err) {
		t.Fatalf("expected 409 conflict, got %v", err)
	}

	// Traefik 404 when disabled and 403 when forbidden.
	srv.SetDisableTraefik(true)
	err = cl.Get(ctx, "/apis/traefik.io/v1alpha1/namespaces/app/middlewares", &map[string]any{})
	if !kube.IsNotFound(err) {
		t.Fatalf("expected 404 for disabled traefik, got %v", err)
	}
	srv.SetForbidden("GET", "secrets", true)
	err = cl.Get(ctx, "/api/v1/namespaces/app/secrets", &map[string]any{})
	if !kube.IsForbidden(err) {
		t.Fatalf("expected 403 for forbidden secrets, got %v", err)
	}
}

func TestValidName(t *testing.T) {
	good := []string{"a", "app", "ferry-state", "v1.2-svc", "de-1"}
	for _, s := range good {
		if !kube.ValidName(s) {
			t.Errorf("ValidName(%q) = false, want true", s)
		}
	}
	bad := []string{"", "-app", "app-", "App", "a/b", "a..b", "a%2fb", "foo_bar"}
	for _, s := range bad {
		if kube.ValidName(s) {
			t.Errorf("ValidName(%q) = true, want false", s)
		}
	}
}
