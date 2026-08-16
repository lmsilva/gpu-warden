package kube

import "testing"

// TestPickCredSource is the regression that sent the first container run into
// a crash loop.
//
// clientcmd.BuildConfigFromFlags only falls back to the in-cluster config when
// the path it receives is empty, so filling in ~/.kube/config before calling
// it means a pod never reaches its ServiceAccount and dies on a file that was
// never going to exist.
func TestPickCredSource(t *testing.T) {
	cases := []struct {
		name       string
		kubeconfig string
		inPod      bool
		want       credSource
	}{
		// The regression: a pod with no kubeconfig must use its own token.
		{"in a pod with nothing named", "", true, credInCluster},
		// The over-correction guard: an explicit path still wins in a pod,
		// because that is how an in-cluster Squire is pointed elsewhere.
		{"in a pod with an explicit kubeconfig", "/etc/other.conf", true, credKubeconfig},
		{"on a laptop with nothing named", "", false, credHomeKubeconfig},
		{"on a laptop with an explicit kubeconfig", "/home/u/.kube/other", false, credKubeconfig},
	}
	for _, tc := range cases {
		if got := pickCredSource(tc.kubeconfig, tc.inPod); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestInPodNeedsBothVariables: one without the other is a shell somebody
// exported half of, not a pod. Guessing wrong sends a laptop at a
// ServiceAccount that is not there, or a pod at a kubeconfig that is not.
func TestInPodNeedsBothVariables(t *testing.T) {
	cases := []struct {
		host, port string
		want       bool
	}{
		{"10.0.0.1", "443", true},
		{"10.0.0.1", "", false},
		{"", "443", false},
		{"", "", false},
	}
	for _, tc := range cases {
		t.Setenv("KUBERNETES_SERVICE_HOST", tc.host)
		t.Setenv("KUBERNETES_SERVICE_PORT", tc.port)
		if got := inPod(); got != tc.want {
			t.Errorf("host=%q port=%q: inPod() = %v, want %v", tc.host, tc.port, got, tc.want)
		}
	}
}
