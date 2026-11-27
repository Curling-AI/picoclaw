package evolution

import "strings"

func skillDraftPromptInstructions() []string {
	return []string{
		"body_or_patch must contain the complete draft body or patch content as plain text.",
		"body_or_patch is the skill the assistant reads when it uses it: write only what that use needs. No notes for reviewers, no provenance, no results of past runs, no names of people or other values copied from the tasks.",
		"If change_kind is create, body_or_patch must be a complete SKILL.md file with exactly two parts: YAML frontmatter and a Markdown body.",
		"The YAML frontmatter must contain only name and description fields.",
		"The description field must and only describe what this skill can do and when to use it.",
		"The deployable Markdown body should only contain what the skill is useful for and how to use it.",
		"The Markdown body is loaded only after the skill triggers, so focus on concise usage guidance and the execution steps needed to complete the task.",
		"Give the shortest path the tasks proved to the final answer: where the inputs are, the exact command or script that produced it (reuse a script or file the tasks created, by its path and the way they ran it), and how to present the result. Leave out the steps the tasks took only to find or inspect the inputs, re-check a result or save notes, unless the person asked for them or the step prevented a failure seen in the tasks. The skill says what the inputs look like, so it needs no step to list the files or check their headers.",
		"Keep every rule the person set or corrected in the tasks (what counts, rates, format) in the step that applies it.",
		"When creating a combined shortcut skill, summarize the functional purpose and result of the provided SKILL.md inputs; do not copy or directly include other skills' instructions.",
		"Extract only the necessary operations from source skills and evidence, such as formulas, ordered transformations, commands, inputs, outputs, and boundary conditions.",
		"The operational part of the generated skill must be directly usable by a future agent without reading the original task records or source skills.",
		"Write the procedure from the tool_steps that worked in the source tasks: the same tools, commands, file layouts and column names, in their order, generalized only where the tasks differ (a file name, a month). A step that failed before one that worked is something to avoid, not a step to repeat.",
		"Keep the skill to what the tasks delivered: no output format, chart, card, extra column or extra step that their final outputs do not show.",
		"Write the name, the description and the body in the language of the person's requests in the source tasks, even though these instructions are in English; the name stays lowercase and hyphenated, without accents.",
		"In the description, say what result the skill produces (for example, which value it sums or counts), so that a request for something else about the same files does not match it.",
	}
}

func skillDraftPromptText() string {
	return strings.Join(skillDraftPromptInstructions(), "\n")
}

func learningTraceReplacer() *strings.Replacer {
	return strings.NewReplacer(
		"## Learned Shortcut Update", "## Shortcut Update",
		"## Learned Evolution", "## Usage Notes",
		"## Learned Pattern", "## Usage Pattern",
		"## Learned Context", "## Procedure Notes",
		"## Source Evidence", "## Validation",
		"## Source Skills", "## Procedure Details",
		"### Source Skills", "### Procedure Details",
		"## Learned Shortcut", "## Shortcut",
		"### Learned Shortcut", "### Shortcut",
		"Learned workflow for ", "Workflow for ",
		"learned workflow for ", "workflow for ",
		"from learned pattern: ", "for: ",
		"Learned task:", "Task:",
		"learned task:", "task:",
		"Learned pattern:", "Pattern:",
		"learned pattern:", "pattern:",
		"Learned from", "Based on",
		"learned from", "based on",
		"Source evidence", "Validation",
		"source evidence", "validation",
		"task records", "validated examples",
		"Task records", "Validated examples",
	)
}

func renderDeployableSkillBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return body
	}
	frontmatter, markdownBody := splitSkillFrontmatter(body)
	if frontmatter != "" {
		body = "---\n" + frontmatter + "\n---\n" + learningTraceReplacer().Replace(strings.TrimLeft(markdownBody, "\n"))
	} else {
		body = learningTraceReplacer().Replace(body)
	}
	body = normalizeDeployableDescription(body)
	return removeDeployOnlyProvenanceLines(body)
}

func normalizeDeployableDescription(body string) string {
	lines := strings.Split(body, "\n")
	inFrontmatter := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if i == 0 && trimmed == "---" {
			inFrontmatter = true
			continue
		}
		if inFrontmatter && trimmed == "---" {
			break
		}
		if !inFrontmatter || !strings.HasPrefix(trimmed, "description:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "description:"))
		value = cleanDeployableDescription(value)
		lines[i] = "description: " + value
		break
	}
	return strings.Join(lines, "\n")
}

func cleanDeployableDescription(description string) string {
	description = strings.TrimSpace(strings.Trim(description, `"'`))
	for _, marker := range []string{
		" for: ",
		" from learned pattern: ",
		" for learned pattern: ",
	} {
		if idx := strings.Index(strings.ToLower(description), marker); idx >= 0 {
			description = strings.TrimSpace(description[idx+len(marker):])
			break
		}
	}
	description = strings.TrimPrefix(description, "Create combined shortcut ")
	description = strings.TrimPrefix(description, "Refresh combined shortcut ")
	description = strings.TrimPrefix(description, "Create shortcut ")
	description = strings.TrimPrefix(description, "Refresh shortcut ")
	description = strings.TrimSpace(description)
	if description == "" {
		return "Use this skill when the task matches its documented workflow."
	}
	return description
}

func sentenceFragment(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "complete the documented workflow"
	}
	runes := []rune(text)
	if len(runes) > 0 && runes[0] >= 'A' && runes[0] <= 'Z' {
		runes[0] = runes[0] + ('a' - 'A')
	}
	return string(runes)
}

func trimAtReadableBoundary(content string, maxLen int) string {
	content = strings.TrimSpace(content)
	runes := []rune(content)
	if content == "" || maxLen <= 0 || len(runes) <= maxLen {
		return content
	}

	cut := maxLen
	searchStart := maxLen - minInt(maxLen/2, 240)
	if searchStart < 0 {
		searchStart = 0
	}
	for i := maxLen; i >= searchStart; i-- {
		switch runes[i-1] {
		case '\n', '.', '!', '?', ';', ':', '。', '！', '？', '；', '：':
			cut = i
			goto done
		}
	}
	for i := maxLen; i >= searchStart; i-- {
		if runes[i-1] == ' ' || runes[i-1] == '\t' {
			cut = i
			goto done
		}
	}

done:
	return strings.TrimRight(strings.TrimSpace(string(runes[:cut])), ".,;:，。；：") + "..."
}

func removeDeployOnlyProvenanceLines(body string) string {
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(lower, "- evidence:") {
			continue
		}
		if strings.HasPrefix(lower, "- validated examples:") {
			continue
		}
		if strings.Contains(lower, "source_record_id") || strings.Contains(lower, "source record") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
