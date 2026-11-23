package javaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A class loaded by the name an XML configuration file gives it (a MyBatis result
// type, a MyBatis Generator plugin) has no caller in Java. The file's naming of it
// is the use: a KindFileRef names each repository type the file spells by its
// fully-qualified name.
func TestJavaXMLConfigTypeRefs(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"gen/src/main/java/com/example/CommentGenerator.java": `package com.example;
public class CommentGenerator {}
`,
		"app/src/main/java/com/example/dto/ProductResult.java": `package com.example.dto;
public class ProductResult {
    public static class Attr {}
}
`,
		"app/src/main/java/com/example/dto/Unnamed.java": `package com.example.dto;
public class Unnamed {}
`,
		"gen/src/main/resources/generatorConfig.xml": `<generatorConfiguration>
  <context id="x">
    <commentGenerator type="com.example.CommentGenerator"/>
    <javaTypeResolver type="org.mybatis.generator.Default"/>
  </context>
</generatorConfiguration>
`,
		"app/src/main/resources/dao/ProductDao.xml": `<mapper namespace="com.example.dao.ProductDao">
  <resultMap id="r" type="com.example.dto.ProductResult">
    <collection property="attrs" ofType="com.example.dto.ProductResult$Attr"/>
  </resultMap>
  <select id="get" resultType="ProductResult" parameterType="java.lang.Long"/>
</mapper>
`,
	})
	named := map[string]bool{}
	for _, r := range factsByKind(ff, facts.KindFileRef) {
		for _, rel := range r.Relations {
			if rel.Kind == facts.RelNames {
				named[rel.Target] = true
			}
		}
	}
	for _, want := range []string{
		"gen/src/main/java/com/example.CommentGenerator",
		"app/src/main/java/com/example/dto.ProductResult",
		"app/src/main/java/com/example/dto.ProductResult.Attr",
	} {
		if !named[want] {
			t.Errorf("%s is named by an XML file but has no reference; got %v", want, named)
		}
	}
	if named["app/src/main/java/com/example/dto.Unnamed"] {
		t.Error("a type no XML file names must not be referenced")
	}
	if len(named) != 3 {
		t.Errorf("only repository types spelled in full bind; got %v", named)
	}
}

// Test resources are test code: an XML file there is not production use.
func TestJavaXMLConfigTypeRefsSkipTestResources(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/src/main/java/com/example/Only.java": `package com.example;
public class Only {}
`,
		"app/src/test/resources/beans.xml": `<bean type="com.example.Only"/>
`,
	})
	if refs := factsByKind(ff, facts.KindFileRef); len(refs) != 0 {
		t.Errorf("XML under test resources must emit no reference; got %v", refs)
	}
}
