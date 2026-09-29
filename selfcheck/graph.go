package selfcheck

import "fmt"

func topoOrder[T any](checks []Check[T], byID map[string]Check[T]) ([]string, error) {
	indegree := make(map[string]int, len(checks))
	dependents := make(map[string][]string)
	for _, check := range checks {
		if _, seen := indegree[check.ID]; !seen {
			indegree[check.ID] = 0
		}
		for _, dependencyID := range check.DependsOn {
			if _, ok := byID[dependencyID]; !ok {
				return nil, fmt.Errorf("check %q depends on unknown check %q", check.ID, dependencyID)
			}
			dependents[dependencyID] = append(dependents[dependencyID], check.ID)
			indegree[check.ID]++
		}
	}

	var queue []string
	for _, check := range checks {
		if indegree[check.ID] == 0 {
			queue = append(queue, check.ID)
		}
	}

	order := make([]string, 0, len(checks))
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, dependentID := range dependents[id] {
			indegree[dependentID]--
			if indegree[dependentID] == 0 {
				queue = append(queue, dependentID)
			}
		}
	}

	if len(order) != len(checks) {
		return nil, fmt.Errorf("dependency cycle detected")
	}
	return order, nil
}
