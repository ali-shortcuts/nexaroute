package httpapi

import (
	"sort"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// Phase 15 (#415 items 2+7): coding-agent workload profiles,
// resolved against the operator's actual configuration. Task
// intelligence mappings live on route profiles, so each profile
// class reports its mappings per route profile. A class never
// claims compatibility the configuration does not prove: the
// capability verdict is computed from the pool deployments'
// DECLARED capabilities — the same operator-owned data the
// model inventory shows.

// longContextThreshold is the declared context window a
// deployment must meet for the long-context profile to count it
// as satisfying the class requirement.
const longContextThreshold = 128000

type codingClass struct {
	id          string
	label       string
	types       []taskprofile.TaskType
	primary     taskprofile.TaskType
	requirement string // tools | vision | reasoning | long_context | none
}

var codingClasses = []codingClass{
	{"primary_coding", "Primary coding", []taskprofile.TaskType{taskprofile.TaskCoding, taskprofile.TaskCodeEdit, taskprofile.TaskToolUse, taskprofile.TaskAgenticTask}, taskprofile.TaskCoding, "tools"},
	{"background", "Inexpensive background tasks", []taskprofile.TaskType{taskprofile.TaskBackground, taskprofile.TaskSimpleChat, taskprofile.TaskDataExtraction, taskprofile.TaskStructuredOutput}, taskprofile.TaskBackground, "none"},
	{"long_context", "Long-context tasks", []taskprofile.TaskType{taskprofile.TaskLongContext, taskprofile.TaskRepositoryAnalysis}, taskprofile.TaskLongContext, "long_context"},
	{"reasoning", "Reasoning-capable tasks", []taskprofile.TaskType{taskprofile.TaskDeepReasoning, taskprofile.TaskArchitectureReasoning, taskprofile.TaskDebugging}, taskprofile.TaskDeepReasoning, "reasoning"},
	{"visual", "Visual tasks", []taskprofile.TaskType{taskprofile.TaskVision}, taskprofile.TaskVision, "vision"},
}

func deploymentCapabilities(cfg config.Config) map[string]config.ModelConfig {
	out := make(map[string]config.ModelConfig)
	for _, p := range cfg.Providers {
		for _, m := range p.Models {
			out[p.ID+"/"+m.ID] = m
		}
	}
	return out
}

func capabilitySatisfied(m config.ModelConfig, requirement string) bool {
	switch requirement {
	case "tools":
		return m.Capabilities.Tools
	case "vision":
		return m.Capabilities.Vision
	case "reasoning":
		return m.Capabilities.Reasoning
	case "long_context":
		return m.ContextWindow >= longContextThreshold
	default:
		return true
	}
}

// codingProfilesSnapshot builds the profile table for the admin
// snapshot. Pure over the config.
func codingProfilesSnapshot(cfg config.Config) map[string]any {
	pools := make(map[string][]string, len(cfg.CandidatePools))
	for _, p := range cfg.CandidatePools {
		pools[p.ID] = p.Deployments
	}
	caps := deploymentCapabilities(cfg)
	classes := make([]map[string]any, 0, len(codingClasses))
	for _, class := range codingClasses {
		types := make([]string, 0, len(class.types))
		for _, t := range class.types {
			types = append(types, string(t))
		}
		mappings := []map[string]any{}
		for _, rp := range cfg.RouteProfiles {
			targetPool := rp.TaskPools[string(class.primary)]
			if targetPool == "" {
				continue
			}
			members := pools[targetPool]
			if members == nil {
				members = []string{}
			}
			satisfied := 0
			unknown := []string{}
			for _, dep := range members {
				m, ok := caps[dep]
				if !ok {
					unknown = append(unknown, dep)
					continue
				}
				if capabilitySatisfied(m, class.requirement) {
					satisfied++
				}
			}
			var verdict string
			switch {
			case class.requirement == "none":
				verdict = "met"
			case len(members) == 0:
				verdict = "unmet"
			case satisfied == len(members):
				verdict = "met"
			case satisfied == 0:
				verdict = "unmet"
			default:
				verdict = "partial"
			}
			sort.Strings(unknown)
			mode := strings.TrimSpace(rp.TaskIntelMode)
			if mode == "" {
				mode = "off"
			}
			mappings = append(mappings, map[string]any{
				"route_profile_id": rp.ID,
				"mode":             mode,
				"target_pool":      targetPool,
				"pool_members":     members,
				"capability":       verdict,
				"capable_members":  satisfied,
				"unknown_members":  unknown,
				"eval_gate":        rp.TaskEvalGates[string(class.primary)],
			})
		}
		classes = append(classes, map[string]any{
			"id":           class.id,
			"label":        class.label,
			"task_types":   types,
			"primary_type": string(class.primary),
			"requirement":  class.requirement,
			"configured":   len(mappings) > 0,
			"mappings":     mappings,
		})
	}
	return map[string]any{"classes": classes}
}
