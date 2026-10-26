package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func graphqlRoute(ff []facts.Fact, name, role string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Name == name && f.Props[facts.PropRole] == role {
			return f, true
		}
	}
	return facts.Fact{}, false
}

// The resolver-map layout of an Express + graphql-tools server: the SDL is a file the
// server loads by path, and each root type is an object in its own module.
func TestGraphQLFieldBindsToItsResolver(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"backend/app.ts": `import { loadSchemaSync } from "@graphql-tools/load";
import { addResolversToSchema } from "@graphql-tools/schema";
import { createHandler } from "graphql-http/lib/use/express";
import { join } from "path";
import resolvers from "./graphql/resolvers";
const schema = loadSchemaSync(join(__dirname, "./graphql/schema.graphql"), { loaders: [] });
export const handler = createHandler({ schema: addResolversToSchema({ schema, resolvers }) });`,
		"backend/graphql/schema.graphql":        "type Query {\n  listAccounts: [Account!]\n}\ntype Mutation {\n  deleteAccount(id: ID!): Boolean\n}\n",
		"backend/graphql/resolvers/Query.ts":    "const Query = {\n  listAccounts(obj: any, args: any) { return [] },\n};\nexport default Query;",
		"backend/graphql/resolvers/Mutation.ts": "const Mutation = {\n  deleteAccount: (obj: any, args: any) => true,\n};\nexport default Mutation;",
		"backend/graphql/resolvers/index.ts":    "import Query from './Query';\nimport Mutation from './Mutation';\nexport default { Query, Mutation };",
		"package.json":                          `{"dependencies": {"graphql-http": "1"}}`,
		"src/machines/accounts.ts":              "import { gql } from '@apollo/client';\nexport const listAccountsQuery = gql`\n  query ListAccounts { listAccounts { id } }\n`;\nexport const typoQuery = gql`\n  query Typo { listAcounts { id } }\n`;",
	}, false)
	for name, resolver := range map[string]string{
		"Query.listAccounts":     "backend/graphql/resolvers.Query.listAccounts",
		"Mutation.deleteAccount": "backend/graphql/resolvers.Mutation.deleteAccount",
	} {
		f, ok := graphqlRoute(ff, name, facts.RoleServer)
		if !ok {
			t.Fatalf("no server field %s read from the SDL the server loads by path", name)
		}
		if !hasRelation(f, facts.RelHandledBy, resolver) {
			t.Errorf("%s not handled_by %s: %v", name, resolver, f.Relations)
		}
	}
	sender, _ := findFact(ff, "src/machines.listAccountsQuery")
	if !hasRelation(sender, facts.RelCalls, "Query.listAccounts") {
		t.Errorf("the operation's variable does not call the field: %v", sender.Relations)
	}
}

// An operation in a .graphql document is sent by the code that imports the document:
// the component, for a .vue file.
func TestGraphQLDocumentOperationIsSentByItsImporter(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/graphql/save_widget.mutation.graphql": "mutation saveWidget($id: ID!) {\n  saveWidget(id: $id) { id }\n}\n",
		"src/components/SaveWidgetButton.vue":      "<script>\nimport saveWidgetMutation from '../graphql/save_widget.mutation.graphql';\nexport default { methods: { run() { return saveWidgetMutation } } }\n</script><template><button /></template>",
		"src/util.js":                              "import saveWidgetMutation from './graphql/save_widget.mutation.graphql';\nexport const send = () => saveWidgetMutation;",
	}, false)
	comp, ok := findFact(ff, "src/components.SaveWidgetButton")
	if !ok {
		t.Fatal("no component")
	}
	if !hasRelation(comp, facts.RelCalls, "Mutation.saveWidget") {
		t.Errorf("the component that imports the document does not send its operation: %v", comp.Relations)
	}
	if !hasTarget(fileRefTargets(ff, "src/util.js"), "Mutation.saveWidget") {
		t.Errorf("a plain module that imports the document does not send its operation")
	}
}

func TestGraphQLInlineResolverMap(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"functions/remote.ts": `import { ApolloServer } from '@apollo/server';
const typeDefs = ` + "`" + `type Query { hello: String }` + "`" + `;
const resolvers = { Query: { hello: () => 'hi' } };
new ApolloServer({ typeDefs, resolvers });`,
	}, false)
	hello, _ := graphqlRoute(ff, "Query.hello", facts.RoleServer)
	if !hasRelation(hello, facts.RelHandledBy, "functions.resolvers.Query.hello") {
		t.Errorf("inline resolver map not bound: %v", hello.Relations)
	}
}

func TestGraphQLAmbiguousResolverIsNotGuessed(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"api/server.ts": `import { ApolloServer } from '@apollo/server';
const typeDefs = ` + "`" + `type Query { user: String }` + "`" + `;
new ApolloServer({ typeDefs });`,
		"api/a.ts": `export const resolvers = { Query: { user: () => 'a' } };`,
		"api/b.ts": `export const other = { Query: { user: () => 'b' } };`,
	}, false)
	f, ok := graphqlRoute(ff, "Query.user", facts.RoleServer)
	if !ok {
		t.Fatal("no server field Query.user")
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelHandledBy {
			t.Errorf("two resolver maps answer Query.user; bound to %s", r.Target)
		}
	}
}

func TestGraphQLCodeFirstFieldBindsToItsMethod(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/users/users.resolver.ts": `import { Resolver, Query } from '@nestjs/graphql';
@Resolver()
export class UsersResolver {
  @Query(() => [String], { name: 'allUsers' })
  findAll() { return [] }
}`,
	}, false)
	f, ok := graphqlRoute(ff, "Query.allUsers", facts.RoleServer)
	if !ok {
		t.Fatal("no code-first field")
	}
	if !hasRelation(f, facts.RelHandledBy, "src/users.UsersResolver.findAll") {
		t.Errorf("decorated field not handled_by its method: %v", f.Relations)
	}
}
