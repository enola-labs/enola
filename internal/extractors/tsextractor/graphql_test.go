package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func detectGraphQLServerUsageForTest(t *testing.T, dir string, files []string) graphqlServerContext {
	t.Helper()
	sources := make(map[string][]byte, len(files))
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		sources[file] = data
	}
	return detectGraphQLServerUsage(files, sources)
}

func TestGraphQLTag_ClientRootFields(t *testing.T) {
	src := []byte("import gql from 'graphql-tag';\nconst Q = gql`\n  query PageStats($id: ID!) {\n    pageViews(companyId: $id) {\n      total\n    }\n    visitors: uniqueVisitors {\n      count\n    }\n  }\n`;\nconst M = gql`\n  mutation {\n    trackEvent(input: {}) {\n      ok\n    }\n  }\n`;\n")
	ff := extractGraphQLTagFacts(src, "src/queries/stats.js")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
		if f.Props[facts.PropRole] != facts.RoleClient || f.Props[facts.PropRouteType] != facts.RouteTypeGraphQL {
			t.Errorf("props = %v", f.Props)
		}
	}
	want := []string{"Query.pageViews", "Query.uniqueVisitors", "Mutation.trackEvent"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v — aliases resolve to the FIELD, nested fields are not roots", names, want)
	}
}

func TestGraphQLTag_ASTIgnoresCommentExamples(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := []byte("// docs: graphql`query Fake { fake }`\nconst real = graphql`query Real { viewer }`;\n")
	if err := os.WriteFile(filepath.Join(dir, "src", "app.ts"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	ff, err := New().Extract(context.Background(), dir, []string{"src/app.ts"})
	if err != nil {
		t.Fatal(err)
	}
	var routes []string
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.Props[facts.PropRouteType] == facts.RouteTypeGraphQL {
			routes = append(routes, f.Name)
		}
	}
	if !reflect.DeepEqual(routes, []string{"Query.viewer"}) {
		t.Fatalf("GraphQL routes = %v, want only Query.viewer", routes)
	}
}

func TestGraphQLClientOps_AnonymousQueryShorthand(t *testing.T) {
	ff := extractGraphQLClientOps("{ viewer { id } aliased: node(id: 1) { id } }", "query.graphql", facts.RouteSourceGraphQLOperation)
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
	}
	want := []string{"Query.viewer", "Query.node"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestGraphQLServerSDL_RootFieldsBecomeServerRoutes(t *testing.T) {
	src := []byte(`import { ApolloServer } from '@apollo/server';
const typeDefs = gql` + "`" + `
  type Query {
    books: [Book]
    book(id: ID!): Book
  }
  type Mutation {
    addBook(title: String!): Book
  }
` + "`" + `;
const server = new ApolloServer({ typeDefs, resolvers });
`)
	ff := extractGraphQLServerSDL(src, "src/server.ts")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
		if f.Props[facts.PropRole] != facts.RoleServer || f.Props[facts.PropRouteType] != facts.RouteTypeGraphQL ||
			f.Props[facts.PropSource] != facts.RouteSourceGraphQLSDL {
			t.Errorf("props = %v", f.Props)
		}
	}
	want := []string{"Query.books", "Query.book", "Mutation.addBook"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestGraphQLServerSDL_PlainStringTypeDefs(t *testing.T) {
	// Apollo Server accepts a plain (non-gql-tagged) template literal too.
	src := []byte(`import { ApolloServer } from '@apollo/server';
const typeDefs = ` + "`" + `
  type Query {
    ping: String
  }
` + "`" + `;
new ApolloServer({ typeDefs });
`)
	ff := extractGraphQLServerSDL(src, "src/server.ts")
	if len(ff) != 1 || ff[0].Name != "Query.ping" {
		t.Fatalf("ff = %+v, want exactly Query.ping", ff)
	}
}

func TestGraphQLServerSDL_InlineObjectShorthand(t *testing.T) {
	// typeDefs declared directly inside the ApolloServer constructor call.
	src := []byte(`new ApolloServer({
  typeDefs: gql` + "`" + `
    type Query {
      viewer: User
    }
  ` + "`" + `,
  resolvers,
});
`)
	ff := extractGraphQLServerSDL(src, "src/server.ts")
	if len(ff) != 1 || ff[0].Name != "Query.viewer" {
		t.Fatalf("ff = %+v, want exactly Query.viewer", ff)
	}
}

func TestGraphQLServerSDL_ExtendedType(t *testing.T) {
	// A modular schema splits its root fields across files with "extend type".
	src := []byte(`new ApolloServer({ typeDefs: gql` + "`" + `
  extend type Query {
    reviews: [Review]
  }
` + "`" + ` });
`)
	ff := extractGraphQLServerSDL(src, "src/reviews-schema.ts")
	if len(ff) != 1 || ff[0].Name != "Query.reviews" {
		t.Fatalf("ff = %+v, want exactly Query.reviews", ff)
	}
}

func TestDetectGraphQLServerUsage_NoServerEmitsNothing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "schema.ts"), []byte("const typeDefs = gql`type Query { unrelated: String }`;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if detectGraphQLServerUsageForTest(t, dir, []string{"schema.ts"}).enabled {
		t.Fatal("schema-only client repository was identified as a GraphQL server")
	}
}

func TestGraphQLServerSDL_ArgsAndDirectivesDoNotProduceExtraFields(t *testing.T) {
	src := []byte(`new ApolloServer({ typeDefs: gql` + "`" + `
  type Query {
    book(id: ID!, format: String = "hardcover"): Book @deprecated(reason: "use books")
  }
` + "`" + ` });
`)
	ff := extractGraphQLServerSDL(src, "src/server.ts")
	if len(ff) != 1 || ff[0].Name != "Query.book" {
		t.Fatalf("ff = %+v, want exactly one field Query.book (args/directives must not surface as fields)", ff)
	}
}

func TestGraphQLServerSDL_MultilineArgumentsDoNotBecomeFields(t *testing.T) {
	src := []byte("const typeDefs = gql`\n  type Query {\n    search(\n      query: String!\n      limit: Int\n    ): Results\n  }\n`;")
	ff := extractGraphQLServerSDL(src, "src/schema.ts")
	if len(ff) != 1 || ff[0].Name != "Query.search" {
		t.Fatalf("ff = %+v, want exactly Query.search", ff)
	}
}

func TestGraphQLServerSDL_BlockDescriptionsAndBracesInStrings(t *testing.T) {
	src := []byte("const typeDefs = gql`\n  type Query {\n    \"\"\"Description: with { braces }\"\"\"\n    greeting(format: String = \"{name}\"): String\n    goodbye: String\n  }\n`;")
	ff := extractGraphQLServerSDL(src, "src/schema.ts")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
	}
	want := []string{"Query.greeting", "Query.goodbye"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestGraphQLServerSDL_BlockDescriptionSchemaExampleIsNotAType(t *testing.T) {
	src := []byte("const typeDefs = gql`\n\"\"\"Example:\n  type Query { fake: String }\n\"\"\"\ntype Query { real: String }\n`;")
	ff := extractGraphQLServerSDL(src, "src/schema.ts")
	if len(ff) != 1 || ff[0].Name != "Query.real" {
		t.Fatalf("description example emitted as schema: %+v", ff)
	}
}

func TestGraphQLServerSDL_RedeclaredFieldAcrossTemplatesIsPreserved(t *testing.T) {
	src := []byte("const firstTypeDefs = gql`type Query { viewer: User }`;\nconst secondTypeDefs = gql`extend type Query { viewer: User }`;")
	ff := extractGraphQLServerSDL(src, "src/schema.ts")
	if len(ff) != 2 || ff[0].Name != "Query.viewer" || ff[1].Name != "Query.viewer" || ff[0].Line == ff[1].Line {
		t.Fatalf("separate declarations must remain separately evidenced: %+v", ff)
	}
}

func TestDetectGraphQLServerUsage_ModularAndGenericConstructor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "server.ts"), []byte("new ApolloServer<MyContext>({ typeDefs });"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.ts"), []byte("export const typeDefs = gql`type Query { ping: String }`;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !detectGraphQLServerUsageForTest(t, dir, []string{"server.ts", "schema.ts"}).enabled {
		t.Fatal("generic ApolloServer constructor was not detected repo-wide")
	}
	ff := extractGraphQLServerSDL([]byte("export const typeDefs = gql`type Query { ping: String }`;"), "schema.ts")
	if len(ff) != 1 || ff[0].Name != "Query.ping" {
		t.Fatalf("modular schema facts = %+v, want Query.ping", ff)
	}
}

func TestGraphQLServerSDL_FrameworkNeutralForms(t *testing.T) {
	src := []byte("const schema = gql`type Query { yoga: String }`;\nconst other = buildSchema(`type Mutation { publish: Boolean }`);")
	ff := extractGraphQLServerSDL(src, "src/schema.ts")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
		if f.Props["framework"] != "graphql-sdl" || f.Props[facts.PropSource] != facts.RouteSourceGraphQLSDL {
			t.Fatalf("framework-neutral props = %v", f.Props)
		}
	}
	want := []string{"Query.yoga", "Mutation.publish"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestGraphQLServerSDL_YogaDocumentedTypeDefinitionsBinding(t *testing.T) {
	src := []byte("import { createSchema } from 'graphql-yoga';\nconst typeDefinitions = `type Query { hello: String! }`;\ncreateSchema({ typeDefs: [typeDefinitions] });")
	ff := extractGraphQLServerSDL(src, "src/schema.ts")
	if len(ff) != 1 || ff[0].Name != "Query.hello" {
		t.Fatalf("Yoga documented typeDefinitions binding = %+v, want Query.hello", ff)
	}
}

func TestGraphQLServerSDL_TypedAndSuffixedBindings(t *testing.T) {
	src := []byte("const schema: string = `type Query { viewer: User }`;\nexport const userTypeDefs: SDL = gql`type Mutation { save: Boolean }`;\nconst gqlSchema = `type Subscription { changed: Event }`;")
	ff := extractGraphQLServerSDL(src, "src/schema.ts")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
	}
	want := []string{"Query.viewer", "Mutation.save", "Subscription.changed"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestGraphQLServerSDL_StandaloneSchemaDocument(t *testing.T) {
	src := []byte("type Query {\n  lastOffers: [Offer!]!\n}\n\ntype Mutation {\n  publish(id: ID!): Boolean!\n}\n")
	ff := extractGraphQLServerSDLDocument(src, "hasura/metadata/actions.graphql")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
	}
	want := []string{"Query.lastOffers", "Mutation.publish"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestDetectGraphQLServerUsage_CommonFrameworks(t *testing.T) {
	for _, source := range []string{
		`import { createYoga } from "graphql-yoga"`,
		`import mercurius from "mercurius"`,
		`import { graphqlHTTP } from "express-graphql"`,
		`import { createHandler } from "graphql-http/lib/use/express"`,
		`makeExecutableSchema({ typeDefs, resolvers })`,
		`buildSchema(typeDefs)`,
		`import { Resolver, Query } from "@nestjs/graphql"`,
		`import { Resolver, Query } from "type-graphql"`,
		`import { queryField } from "nexus"`,
		`import SchemaBuilder from "@pothos/core"`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "server.ts"), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
		if !detectGraphQLServerUsageForTest(t, dir, []string{"server.ts"}).enabled {
			t.Errorf("server signal not detected in %q", source)
		}
	}
}

func TestGraphQLCodeFirst_NestAndTypeGraphQLDecorators(t *testing.T) {
	src := []byte(`import { Resolver, Query, Mutation, Subscription } from "@nestjs/graphql";
@Resolver()
class BooksResolver {
  @Query(() => Book)
  book() {}

  @Mutation(() => Book, { name: "publishBook" })
  publish() {}

  @Subscription("bookChanged")
  changed() {}
}`)
	ff := extractGraphQLCodeFirst(src, "src/books.resolver.ts")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
	}
	want := []string{"Query.book", "Mutation.publishBook", "Subscription.bookChanged"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestGraphQLCodeFirst_ComputedDecoratorNameIsNotInvented(t *testing.T) {
	src := []byte("import { Query } from \"type-graphql\";\n" +
		"class ResourceResolver {\n" +
		"  @Query(() => Resource, { name: `${resourceName}s` }) getAll() {}\n" +
		"  @Query(`${resourceName}`) getOne() {}\n" +
		"}")
	if ff := extractGraphQLCodeFirst(src, "src/resource.resolver.ts"); len(ff) != 0 {
		t.Fatalf("computed GraphQL name emitted a literal or fallback route: %+v", ff)
	}
}

func TestGraphQLCodeFirst_NexusAndPothosFields(t *testing.T) {
	src := []byte(`import { queryField, mutationField } from "nexus";
import SchemaBuilder from "@pothos/core";
queryField("viewer", { type: "User", resolve() {} });
mutationField("saveUser", { type: "User", resolve() {} });
const builder = new SchemaBuilder({});
builder.queryField("health", (t) => t.string({ resolve: () => "ok" }));
builder.mutationField("publish", (t) => t.boolean({ resolve: () => true }));`)
	ff := extractGraphQLCodeFirst(src, "src/schema.ts")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
	}
	want := []string{"Query.viewer", "Mutation.saveUser", "Query.health", "Mutation.publish"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestGraphQLCodeFirst_NexusRootDefinitionsAndCallbacks(t *testing.T) {
	src := []byte(`import { queryType, mutationType, subscriptionType, extendType, queryField } from "nexus";
export const Query = queryType({
  definition(t) {
    t.field("viewer", { type: "User" });
    t.nonNull.string("status", { resolve: () => "ok" });
		t.nonNull.list.field("drafts", { type: "Post" });
		t.customScalar("score", { resolve: () => 1 });
		t.implements("Node");
  },
});
export const Mutation = mutationType({ definition(t) { t.boolean("ok", { resolve: () => true }); } });
export const Subscription = subscriptionType({ definition: (t) => { t.field("events", { type: "Event" }); } });
export const MoreQueryFields = extendType({
  type: "Query",
  definition(t) { t.int("protectedField", { resolve: () => 1 }); },
});
export const Users = queryField(t => {
  t.connectionField("users", { type: "User", nodes() { return []; } });
});`)
	ff := extractGraphQLCodeFirst(src, "src/schema.ts")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
	}
	want := []string{"Query.viewer", "Query.status", "Query.drafts", "Query.score", "Mutation.ok", "Subscription.events", "Query.protectedField", "Query.users"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestGraphQLCodeFirst_RequiresPackageProvenance(t *testing.T) {
	for _, src := range []string{
		`class Store { @Query() rows() {} }
builder.queryField("notGraphQL", () => value);`,
		`// import { Query } from "@nestjs/graphql"
class Store { @Query() rows() {} }`,
		`import { Resolver } from "type-graphql";
function Query(): MethodDecorator { return () => {}; }
class Store { @Query() rows() {} }`,
	} {
		if ff := extractGraphQLCodeFirst([]byte(src), "src/store.ts"); len(ff) != 0 {
			t.Fatalf("unrelated APIs emitted GraphQL routes: %+v", ff)
		}
	}
}

func TestGraphQLCodeFirst_ImportAliasesSubpathsAndScopes(t *testing.T) {
	src := []byte(`import { Query as GQuery } from "@nestjs/graphql/dist/decorators";
import { queryType as qt, queryField as qf } from "nexus/dist/index";
import SchemaBuilder from "@pothos/core";
class Resolver { @GQuery(() => String) health() {} }
qt({ definition(t) {
  t.string("viewer");
  function nested(t) { t.string("notARootField"); }
} });
qf("node", { type: "Node" });
const builder = new SchemaBuilder({});
const unrelated = { queryField(name) {} };
builder.queryField("pothosHealth", t => t.string({ resolve: () => "ok" }));
unrelated.queryField("notPothos", () => {});`)
	ff := extractGraphQLCodeFirst(src, "src/schema.ts")
	var names []string
	for _, f := range ff {
		names = append(names, f.Name)
	}
	want := []string{"Query.health", "Query.viewer", "Query.node", "Query.pothosHealth"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestGraphQLClientCalls_GraphQLRequestAndPlainFetch(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"graphql-request", `import { request } from "graphql-request";
const document = ` + "`query Viewer { viewer { id } }`" + `;
request(endpoint, document);`, "Query.viewer"},
		{"plain fetch", `fetch("/graphql", { method: "POST", body: JSON.stringify({
  query: "mutation Save { save { id } }"
}) });`, "Mutation.save"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ff := extractGraphQLClientCallFacts([]byte(tc.src), "src/client.ts")
			if len(ff) != 1 || ff[0].Name != tc.want {
				t.Fatalf("facts = %+v, want exactly %s", ff, tc.want)
			}
		})
	}
}

func TestGraphQLClientCalls_RequireClientProvenance(t *testing.T) {
	for _, src := range []string{
		"const documentation = `query Example { fake }`;",
		"// import { request } from 'graphql-request'\nconst documentation = `query Example { fake }`;",
		"import { gql } from 'urql';\nconst documentation = `query Example { fake }`;",
	} {
		if ff := extractGraphQLClientCallFacts([]byte(src), "src/docs.ts"); len(ff) != 0 {
			t.Fatalf("unproven operation-looking string emitted routes: %+v", ff)
		}
	}
}

func TestDetectGraphQLServerUsage_CommentsDoNotActivateServer(t *testing.T) {
	dir := t.TempDir()
	src := []byte("/** Example: const schema = buildSchema(`type Query { fake: String }`); */\nexport function validate() {}")
	if err := os.WriteFile(filepath.Join(dir, "library.ts"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if detectGraphQLServerUsageForTest(t, dir, []string{"library.ts"}).enabled {
		t.Fatal("buildSchema documentation example activated GraphQL server detection")
	}
	if ff := extractGraphQLServerSDL(src, "library.ts"); len(ff) != 0 {
		t.Fatalf("documentation example emitted server routes: %+v", ff)
	}
}

func TestDetectGraphQLServerUsage_StandaloneSDLRequiresProvenance(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"server.ts":                `import schema from "./schema.graphql"; buildSchema(schema)`,
		"schema.graphql":           `type Query { real: String }`,
		"benchmark/github.graphql": `type Query { fixture: String }`,
	}
	for path, body := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := detectGraphQLServerUsageForTest(t, dir, []string{"server.ts", "schema.graphql", "benchmark/github.graphql"})
	if !ctx.sdlDocuments["schema.graphql"] {
		t.Fatal("server-imported schema.graphql lacks provenance")
	}
	if ctx.sdlDocuments["benchmark/github.graphql"] {
		t.Fatal("unreferenced benchmark schema was promoted to a server schema")
	}
}

func TestGraphQLDoc_OperationFileAndSchemaCopy(t *testing.T) {
	ops := extractGraphQLClientOps("query Profile {\n  me {\n    name\n  }\n}\n", "graphql/Profile.graphql", facts.RouteSourceGraphQLOperation)
	if len(ops) != 1 || ops[0].Name != "Query.me" {
		t.Fatalf("ops = %+v", ops)
	}
	schema := extractGraphQLClientOps("type Query {\n  me: User\n}\n", "graphql/schema.graphql", facts.RouteSourceGraphQLOperation)
	if len(schema) != 0 {
		t.Errorf("a schema COPY emitted %v — type-definition blocks are codegen inputs, not client operations", schema)
	}
}

func TestGraphQLTag_FragmentInterpolation(t *testing.T) {
	src := []byte("const Q = gql`\n  query Feed {\n    stories {\n      ...StoryFields\n    }\n  }\n  ${STORY_FIELDS}\n`;\n")
	ff := extractGraphQLTagFacts(src, "src/feed.js")
	if len(ff) != 1 || ff[0].Name != "Query.stories" {
		t.Fatalf("interpolated-fragment operation = %+v, want exactly Query.stories", ff)
	}
}

func TestReactNavDetect_MonorepoExampleApp(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "example"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"framework","workspaces":["example"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "example", "package.json"), []byte(`{"dependencies":{"@react-navigation/native":"^6.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !detectReactNavigation(dir) {
		t.Error("a monorepo example app one level down must trigger detection")
	}
}

func TestGraphQLDocDetect_NoTSRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "graphql"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(dir, "graphql", "CurrentProfile.graphql")
	if err := os.WriteFile(doc, []byte("query CurrentProfile {\n  currentProfile {\n    id\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New()
	found, err := e.Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("a repo carrying .graphql operation documents with no TS root must detect — the Swift Apollo case")
	}
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "main.swift"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	found, err = e.Detect(bare)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a repo with neither TS markers nor GraphQL documents must not detect")
	}
}

func TestGraphQLDocDetect_IgnoresNestedBuildOutputs(t *testing.T) {
	for _, dir := range []string{"dist", "build", "out", ".next", "target", "vendor", "node_modules"} {
		if hasGraphQLDocs([]string{"packages/foo/" + dir + "/stray.graphql"}) {
			t.Errorf("%s GraphQL output activated the TypeScript extractor", dir)
		}
	}
	if !hasGraphQLDocs([]string{"packages/foo/src/live.graphql"}) {
		t.Fatal("source GraphQL document did not activate the extractor")
	}
}

func TestGraphQLDocDetect_GradleNestedDocuments(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "build.gradle"), []byte("plugins {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "app", "src", "main", "graphql")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "Profile.graphql"), []byte("query Profile {\n  me {\n    id\n  }\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := New()
	found, err := e.Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("Gradle-nested Apollo documents (app/src/main/graphql/) must detect — the Gradle-layout case")
	}
}

// A gql tag whose interpolation carries its own template literal. The old
// pattern ended the body at the first inner backtick, which cut the document in
// half: the visible cost was a JavaScript variable read as the first root
// field, the quiet one was every real field after the cut never being seen.
func TestGraphQLTag_InterpolatedTemplateInsideTheDocument(t *testing.T) {
	src := []byte("const q = (inOverview = false) => gql`\n" +
		"  query DeviceQuery($dateRange: DateRangeAttributes!) {\n" +
		"    deviceQuery: ${\n" +
		"      inOverview\n" +
		"        ? `sessionPageviewQuery(dateRange: $dateRange) { total }`\n" +
		"        : `combinedQuery(dateRange: $dateRange) { total }`\n" +
		"    }\n" +
		"    candidates {\n" +
		"      id\n" +
		"    }\n" +
		"  }\n" +
		"`;\n")

	var names []string
	for _, f := range extractGraphQLTagFacts(src, "q.ts") {
		names = append(names, f.Name)
	}
	if len(names) != 1 || names[0] != "Query.candidates" {
		t.Errorf("want only the statically named root field, got %v", names)
	}
}

// The word `graphql` followed by a backtick is not always a tag: here the
// backtick CLOSES an ordinary template literal. Reading it as an opening one
// ran the scan to end of file and turned an Apollo options object into a
// document — Query.variables and Query.network, from a file with no GraphQL in
// it at all.
func TestGraphQLTag_WordInAPathIsNotATag(t *testing.T) {
	src := []byte("const link = () => ({\n" +
		"  uri: `${this.config.railsURL}/graphql`,\n" +
		"  defaultOptions: {\n" +
		"    query: {\n" +
		"      fetchPolicy: 'network-only',\n" +
		"      variables,\n" +
		"    },\n" +
		"  },\n" +
		"});\n")

	if got := extractGraphQLTagFacts(src, "apollo.ts"); len(got) != 0 {
		t.Errorf("no tag here, so no facts; got %+v", got)
	}
}

// A builder another file creates is still Pothos's, and the plural forms declare a
// field per key of the object their callback returns.
func TestGraphQLCodeFirst_PothosBuilderImportedAndPluralFields(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/builder.ts": `import SchemaBuilder from "@pothos/core";
const builder = new SchemaBuilder<{ Context: {} }>({});
export { builder };`,
		"src/schema/stripe.ts": `import { builder } from "../builder";
builder.queryFields((t) => ({
  stripe: t.field({ type: "Stripe", resolve: () => ({}) }),
  "health": t.string({ resolve: () => "ok" }),
}));
builder.mutationFields((t) => ({ stripe: t.field({ type: "StripeMutations", resolve: () => ({}) }) }));`,
		"src/schema/other.ts": `import { builder } from "./not-the-builder";
builder.queryFields((t) => ({ ghost: t.field({}) }));`,
		"src/schema/not-the-builder.ts": `export const builder = { queryFields(_: unknown) {} };`,
	}, false)
	got := map[string]bool{}
	for _, f := range ff {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropSource) == facts.RouteSourceGraphQLPothos {
			got[f.Name] = true
		}
	}
	want := map[string]bool{"Query.stripe": true, "Query.health": true, "Mutation.stripe": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pothos fields %v, want %v", got, want)
	}
}
