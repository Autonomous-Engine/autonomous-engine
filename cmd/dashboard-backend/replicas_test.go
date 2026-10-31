package main

import (
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestCheckReplicaConfig(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    int
		wantErr string
	}{
		{"unset means one replica, no Redis needed", map[string]string{}, 1, ""},
		{"one replica without Redis", map[string]string{"DASHBOARD_REPLICAS": "1"}, 1, ""},
		{"one replica with Redis", map[string]string{"DASHBOARD_REPLICAS": "1", "REDIS_URL": "redis://r:6379/0"}, 1, ""},
		{"three replicas with Redis", map[string]string{"DASHBOARD_REPLICAS": "3", "REDIS_URL": "redis://r:6379/0"}, 3, ""},
		{"two replicas without Redis refused", map[string]string{"DASHBOARD_REPLICAS": "2"}, 0, "REDIS_URL is not set"},
		{"blank REDIS_URL counts as unset", map[string]string{"DASHBOARD_REPLICAS": "2", "REDIS_URL": "  "}, 0, "REDIS_URL is not set"},
		{"not a number refused", map[string]string{"DASHBOARD_REPLICAS": "two"}, 0, "DASHBOARD_REPLICAS"},
		{"zero refused", map[string]string{"DASHBOARD_REPLICAS": "0"}, 0, "DASHBOARD_REPLICAS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checkReplicaConfig(envMap(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("replicas = %d, want %d", got, tc.want)
			}
		})
	}
}
