package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ngaut/agent-git-service/internal/db"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

const delegatedGraphQLBodyLimit = 1 << 20

type delegatedGraphQLRequest struct {
	OperationName string         `json:"operationName"`
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
}

// delegatedSessionGraphQLRequestAllowed admits only repository-bound PR reads
// emitted by the filtered gh surface. The query shape is checked before the
// GraphQL resolver can read any resource.
func delegatedSessionGraphQLRequestAllowed(session db.DelegatedAgentSession, r *http.Request) bool {
	if strings.ToLower(strings.TrimSpace(session.OperationName)) != "pr.read" || r.Method != http.MethodPost || r.Body == nil {
		return false
	}
	if r.URL.Path != "/api/graphql" && r.URL.Path != "/graphql" {
		return false
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, delegatedGraphQLBodyLimit+1))
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil || len(body) == 0 || len(body) > delegatedGraphQLBodyLimit {
		return false
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var request delegatedGraphQLRequest
	if err := decoder.Decode(&request); err != nil {
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return false
	}

	document, err := parser.ParseQuery(&ast.Source{Input: request.Query})
	if err != nil || document == nil || len(document.Operations) != 1 {
		return false
	}
	operation := document.Operations[0]
	if operation.Operation != ast.Query || (request.OperationName != "" && request.OperationName != operation.Name) {
		return false
	}
	repositoryParts := strings.Split(session.Repository.FullName, "/")
	if len(repositoryParts) != 2 || repositoryParts[0] == "" || repositoryParts[1] == "" {
		return false
	}

	switch operation.Name {
	case "PullRequestByNumber":
		return delegatedSessionGraphQLPRByNumberAllowed(session, request, document, operation, repositoryParts)
	case "PullRequestList":
		return delegatedSessionGraphQLPRListAllowed(request, operation, repositoryParts)
	default:
		return false
	}
}

func delegatedSessionGraphQLPRByNumberAllowed(session db.DelegatedAgentSession, request delegatedGraphQLRequest, document *ast.QueryDocument, operation *ast.OperationDefinition, repositoryParts []string) bool {
	if len(document.Fragments) != 0 {
		return false
	}
	if len(operation.VariableDefinitions) != 3 || len(operation.Directives) != 0 ||
		!delegatedGraphQLVariableDefinition(operation, "owner", "String!") ||
		!delegatedGraphQLVariableDefinition(operation, "repo", "String!") ||
		!delegatedGraphQLVariableDefinition(operation, "pr_number", "Int!") {
		return false
	}

	repositoryField, ok := delegatedSingleGraphQLField(operation.SelectionSet, "repository")
	if !ok || len(repositoryField.Arguments) != 2 ||
		!delegatedGraphQLVariableArgument(repositoryField, "owner", "owner") ||
		!delegatedGraphQLVariableArgument(repositoryField, "name", "repo") {
		return false
	}
	pullRequestField, ok := delegatedSingleGraphQLField(repositoryField.SelectionSet, "pullRequest")
	if !ok || len(pullRequestField.Arguments) != 1 || len(pullRequestField.SelectionSet) == 0 ||
		!delegatedGraphQLVariableArgument(pullRequestField, "number", "pr_number") {
		return false
	}

	if len(request.Variables) != 3 {
		return false
	}
	owner, ownerOK := request.Variables["owner"].(string)
	repository, repositoryOK := request.Variables["repo"].(string)
	pullRequestNumber, numberOK := request.Variables["pr_number"].(json.Number)
	if !ownerOK || !repositoryOK || !numberOK || owner != repositoryParts[0] || repository != repositoryParts[1] {
		return false
	}
	number, err := strconv.ParseUint(pullRequestNumber.String(), 10, 64)
	return err == nil && number > 0 && session.OperationConstraints["pull_request_number"] == pullRequestNumber.String()
}

func delegatedSessionGraphQLPRListAllowed(request delegatedGraphQLRequest, operation *ast.OperationDefinition, repositoryParts []string) bool {
	if len(operation.VariableDefinitions) != 7 || len(operation.Directives) != 0 ||
		!delegatedGraphQLVariableDefinition(operation, "owner", "String!") ||
		!delegatedGraphQLVariableDefinition(operation, "repo", "String!") ||
		!delegatedGraphQLVariableDefinition(operation, "limit", "Int!") ||
		!delegatedGraphQLVariableDefinition(operation, "endCursor", "String") ||
		!delegatedGraphQLVariableDefinition(operation, "baseBranch", "String") ||
		!delegatedGraphQLVariableDefinition(operation, "headBranch", "String") ||
		!delegatedGraphQLVariableDefinitionWithEnumDefault(operation, "state", "[PullRequestState!]", "OPEN") {
		return false
	}

	repositoryField, ok := delegatedSingleGraphQLField(operation.SelectionSet, "repository")
	if !ok || len(repositoryField.Arguments) != 2 ||
		!delegatedGraphQLVariableArgument(repositoryField, "owner", "owner") ||
		!delegatedGraphQLVariableArgument(repositoryField, "name", "repo") {
		return false
	}
	pullRequestsField, ok := delegatedSingleGraphQLField(repositoryField.SelectionSet, "pullRequests")
	if !ok || len(pullRequestsField.Arguments) != 6 ||
		!delegatedGraphQLVariableArgument(pullRequestsField, "states", "state") ||
		!delegatedGraphQLVariableArgument(pullRequestsField, "baseRefName", "baseBranch") ||
		!delegatedGraphQLVariableArgument(pullRequestsField, "headRefName", "headBranch") ||
		!delegatedGraphQLVariableArgument(pullRequestsField, "first", "limit") ||
		!delegatedGraphQLVariableArgument(pullRequestsField, "after", "endCursor") ||
		!delegatedGraphQLPRListOrder(pullRequestsField) ||
		len(pullRequestsField.SelectionSet) == 0 {
		return false
	}

	owner, ownerOK := request.Variables["owner"].(string)
	repository, repositoryOK := request.Variables["repo"].(string)
	limit, limitOK := request.Variables["limit"].(json.Number)
	if !ownerOK || !repositoryOK || !limitOK || owner != repositoryParts[0] || repository != repositoryParts[1] {
		return false
	}
	limitValue, err := strconv.ParseUint(limit.String(), 10, 64)
	if err != nil || limitValue == 0 {
		return false
	}
	for key, value := range request.Variables {
		switch key {
		case "owner", "repo", "limit":
		case "endCursor", "baseBranch", "headBranch":
			if value != nil {
				if _, ok := value.(string); !ok {
					return false
				}
			}
		case "state":
			states, ok := value.([]any)
			if !ok || len(states) == 0 {
				return false
			}
			for _, state := range states {
				stateName, ok := state.(string)
				if !ok || (stateName != "OPEN" && stateName != "CLOSED" && stateName != "MERGED") {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

func delegatedGraphQLVariableDefinition(operation *ast.OperationDefinition, name, typeName string) bool {
	definition := operation.VariableDefinitions.ForName(name)
	return definition != nil && definition.Type != nil && definition.Type.String() == typeName && definition.DefaultValue == nil && len(definition.Directives) == 0
}

func delegatedGraphQLVariableDefinitionWithEnumDefault(operation *ast.OperationDefinition, name, typeName, defaultValue string) bool {
	definition := operation.VariableDefinitions.ForName(name)
	return definition != nil && definition.Type != nil && definition.Type.String() == typeName && len(definition.Directives) == 0 &&
		definition.DefaultValue != nil && definition.DefaultValue.Kind == ast.EnumValue && definition.DefaultValue.Raw == defaultValue
}

func delegatedSingleGraphQLField(selectionSet ast.SelectionSet, name string) (*ast.Field, bool) {
	if len(selectionSet) != 1 {
		return nil, false
	}
	field, ok := selectionSet[0].(*ast.Field)
	return field, ok && field.Name == name && (field.Alias == "" || field.Alias == name) && len(field.Directives) == 0
}

func delegatedGraphQLVariableArgument(field *ast.Field, argumentName, variableName string) bool {
	argument := field.Arguments.ForName(argumentName)
	return argument != nil && argument.Value != nil && argument.Value.Kind == ast.Variable && argument.Value.Raw == variableName
}

func delegatedGraphQLPRListOrder(field *ast.Field) bool {
	argument := field.Arguments.ForName("orderBy")
	if argument == nil || argument.Value == nil || argument.Value.Kind != ast.ObjectValue || len(argument.Value.Children) != 2 {
		return false
	}
	orderField := argument.Value.Children.ForName("field")
	direction := argument.Value.Children.ForName("direction")
	return orderField != nil && orderField.Kind == ast.EnumValue && orderField.Raw == "CREATED_AT" &&
		direction != nil && direction.Kind == ast.EnumValue && direction.Raw == "DESC"
}
