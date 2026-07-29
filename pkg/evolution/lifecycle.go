package evolution

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/skills"
)

type LifecycleRunSummary struct {
	EvaluatedProfiles    int
	TransitionedProfiles int
	DeletedSkills        int
}

func NextLifecycleState(profile SkillProfile, now time.Time) SkillStatus {
	if profile.Origin == "manual" || profile.LastUsedAt.IsZero() {
		return profile.Status
	}

	idle := now.Sub(profile.LastUsedAt)
	switch profile.Status {
	case SkillStatusActive:
		if idle > 90*24*time.Hour && profile.RetentionScore < 0.3 {
			return SkillStatusCold
		}
	case SkillStatusCold:
		if idle > 180*24*time.Hour && profile.RetentionScore < 0.2 {
			return SkillStatusArchived
		}
	case SkillStatusArchived:
		if idle > 365*24*time.Hour && profile.RetentionScore < 0.1 {
			return SkillStatusDeleted
		}
	}

	return profile.Status
}

func ApplyLifecycleState(paths Paths, profile SkillProfile, next SkillStatus) error {
	if next != SkillStatusDeleted {
		return nil
	}

	workspace := profile.WorkspaceID
	if workspace == "" {
		workspace = inferWorkspaceFromPaths(paths)
	}
	if workspace == "" {
		return fmt.Errorf("resolve lifecycle delete workspace for skill %q: workspace is required", profile.SkillName)
	}
	if err := skills.ValidateSkillName(profile.SkillName); err != nil {
		return fmt.Errorf("resolve lifecycle delete skill name: %w", err)
	}

	skillPath := filepath.Join(workspace, "skills", profile.SkillName, "SKILL.md")
	return trashSkillFile(paths, profile.SkillName, skillPath)
}

// maxLifecycleDeletesPerRun limita quantas skills um único run pode aposentar.
//
// A transição só dispara com 365 dias de ociosidade, então mais que um punhado
// por run não é o ciclo de vida funcionando: é um relógio errado, um restore de
// backup com mtime zerado, ou perfis importados em lote. Nesses casos parar e
// avisar é melhor que apagar tudo e descobrir depois.
const maxLifecycleDeletesPerRun = 5

// trashSkillFile move a skill para a lixeira em vez de apagar.
//
// O lifecycle apagava com os.Remove direto. Só que a decisão de aposentar vem de
// um retention score alimentado por um juiz de sucesso heurístico/LLM — um sinal
// ruidoso —, e o arquivo é trabalho que o próprio sistema gerou. Deletar sem
// rede transforma um erro de pontuação em perda definitiva. Com Loops isso
// piora: agora o usuário VÊ essas skills, então o sumiço é visível e
// inexplicável. (seucaranguejo fork)
func trashSkillFile(paths Paths, skillName, skillPath string) error {
	data, err := os.ReadFile(skillPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	trashDir := filepath.Join(paths.RootDir, "trash")
	if err := os.MkdirAll(trashDir, 0o755); err != nil {
		return fmt.Errorf("create lifecycle trash dir: %w", err)
	}
	// Nome do arquivo carrega a data para dois ciclos de vida do mesmo nome não
	// se sobrescreverem na lixeira.
	trashPath := filepath.Join(trashDir, fmt.Sprintf("%s.%s.SKILL.md",
		skillName, time.Now().UTC().Format("20060102T150405Z")))
	if err := os.WriteFile(trashPath, data, 0o644); err != nil {
		return fmt.Errorf("write lifecycle trash copy: %w", err)
	}

	if err := os.Remove(skillPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func RunLifecycleOnce(store *Store, paths Paths, workspace string, now time.Time) (LifecycleRunSummary, error) {
	if store == nil {
		return LifecycleRunSummary{}, nil
	}

	profiles, err := store.LoadProfiles()
	if err != nil {
		return LifecycleRunSummary{}, err
	}

	summary := LifecycleRunSummary{}
	for _, profile := range profiles {
		if !profileBelongsToWorkspace(paths, workspace, profile) {
			continue
		}

		summary.EvaluatedProfiles++
		next := NextLifecycleState(profile, now)
		if next == profile.Status {
			continue
		}

		// O teto vale por RUN, não por perfil: um run que quer apagar muita
		// coisa de uma vez é o sintoma que interessa. Os perfis restantes ficam
		// como estão e voltam a ser avaliados no run seguinte.
		if next == SkillStatusDeleted && summary.DeletedSkills >= maxLifecycleDeletesPerRun {
			logger.WarnCF("evolution", "Lifecycle delete cap reached — skipping remaining deletions", map[string]any{
				"cap":       maxLifecycleDeletesPerRun,
				"workspace": workspace,
				"skill":     profile.SkillName,
			})
			continue
		}

		if err := ApplyLifecycleState(paths, profile, next); err != nil {
			return summary, err
		}
		profile.VersionHistory = append(profile.VersionHistory, SkillVersionEntry{
			Version:   profile.CurrentVersion,
			Action:    "lifecycle:" + string(next),
			Timestamp: now,
			Summary:   fmt.Sprintf("lifecycle transition: %s -> %s", profile.Status, next),
		})
		profile.Status = next
		if err := store.SaveProfile(profile); err != nil {
			return summary, err
		}

		summary.TransitionedProfiles++
		if next == SkillStatusDeleted {
			summary.DeletedSkills++
		}
	}

	return summary, nil
}

func inferWorkspaceFromPaths(paths Paths) string {
	root := filepath.Clean(paths.RootDir)
	if filepath.Base(root) != "evolution" {
		return ""
	}
	stateDir := filepath.Dir(root)
	if filepath.Base(stateDir) != "state" {
		return ""
	}
	return filepath.Dir(stateDir)
}

func profileBelongsToWorkspace(paths Paths, workspace string, profile SkillProfile) bool {
	if profile.WorkspaceID == workspace {
		return true
	}
	return profile.WorkspaceID == "" && usesDefaultWorkspaceState(paths, workspace)
}

func usesDefaultWorkspaceState(paths Paths, workspace string) bool {
	return paths.RootDir == NewPaths(workspace, "").RootDir
}
