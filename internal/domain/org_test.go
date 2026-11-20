package domain

import (
	"errors"
	"testing"
	"time"
)

func TestOrgQuotasValidate(t *testing.T) {
	var nilQuotas *OrgQuotas
	if err := nilQuotas.Validate(); err != nil {
		t.Fatalf("nil Validate = %v", err)
	}

	cases := []struct {
		name   string
		quotas OrgQuotas
		want   bool
	}{
		{"zero is valid", OrgQuotas{}, false},
		{"soft above hard", OrgQuotas{MaxGroups: QuotaThreshold{Soft: 5, Hard: 1}}, true},
		{"negative soft", OrgQuotas{MaxProjects: QuotaThreshold{Soft: -1}}, true},
		{"negative hard", OrgQuotas{MaxMembers: QuotaThreshold{Hard: -1}}, true},
		{"negative retention", OrgQuotas{MaxPipelineRetention: -time.Second}, true},
		{"equal thresholds", OrgQuotas{MaxEngines: QuotaThreshold{Soft: 3, Hard: 3}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.quotas.Validate()
			if tc.want && !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if !tc.want && err != nil {
				t.Fatalf("unexpected err = %v", err)
			}
		})
	}
}

func TestOrgQuotasEffective(t *testing.T) {
	defaults := OrgQuotas{
		MaxGroups:             QuotaThreshold{Soft: 5, Hard: 10},
		MaxProjects:           QuotaThreshold{Hard: 20},
		MaxMembers:            QuotaThreshold{Hard: 30},
		MaxConcurrentSessions: QuotaThreshold{Hard: 40},
		MaxEngines:            QuotaThreshold{Hard: 50},
		MaxCacheBytes:         QuotaThreshold{Hard: 60},
		MaxPipelineRetention:  48 * time.Hour,
	}
	own := OrgQuotas{
		MaxGroups:            QuotaThreshold{Hard: 3},
		MaxProjects:          QuotaThreshold{Soft: 2},
		MaxPipelineRetention: time.Hour,
	}
	eff := own.Effective(defaults)
	if eff.MaxGroups != own.MaxGroups || eff.MaxProjects != own.MaxProjects {
		t.Fatalf("org overrides lost: %+v", eff)
	}
	if eff.MaxMembers != defaults.MaxMembers || eff.MaxConcurrentSessions != defaults.MaxConcurrentSessions {
		t.Fatalf("defaults not inherited: %+v", eff)
	}
	if eff.MaxEngines != defaults.MaxEngines || eff.MaxCacheBytes != defaults.MaxCacheBytes {
		t.Fatalf("defaults not inherited: %+v", eff)
	}
	if eff.MaxPipelineRetention != time.Hour {
		t.Fatalf("retention = %v, want own 1h", eff.MaxPipelineRetention)
	}

	empty := OrgQuotas{}
	inherited := empty.Effective(defaults)
	if inherited.MaxGroups != defaults.MaxGroups || inherited.MaxPipelineRetention != 48*time.Hour {
		t.Fatalf("empty org did not inherit defaults: %+v", inherited)
	}
}

func TestOrgHasFeature(t *testing.T) {
	var nilOrg *Org
	if nilOrg.HasFeature("CLOUD_CHECKS") {
		t.Fatal("nil org must not report features")
	}
	o := &Org{Features: []string{"CLOUD_CHECKS", "CLOUD_MODULES"}}
	if !o.HasFeature("CLOUD_CHECKS") || !o.HasFeature("CLOUD_MODULES") {
		t.Fatalf("enabled features not reported: %+v", o.Features)
	}
	if o.HasFeature("UNKNOWN") {
		t.Fatal("unknown feature reported as enabled")
	}
}
