package javaextractor

import (
	"slices"
	"testing"
)

func TestJavaLibraryCall(t *testing.T) {
	tests := []struct {
		typ, method string
		want        int
	}{
		{"org.springframework.jdbc.core.JdbcTemplate", "update", javaCallIO},
		{"org.springframework.jdbc.core.JdbcTemplate", "getDataSource", javaCallUnknown},
		{"jakarta.persistence.EntityManager", "find", javaCallIO},
		// It builds the query that Query.getResultList runs.
		{"jakarta.persistence.EntityManager", "createQuery", javaCallUnknown},
		{"jakarta.persistence.TypedQuery", "getResultList", javaCallIO},
		{"java.sql.PreparedStatement", "setLong", javaCallUnknown},
		{"java.sql.PreparedStatement", "executeBatch", javaCallIO},
		// Every method of a client, but what configures or closes it.
		{"org.apache.kafka.clients.admin.AdminClient", "listConsumerGroups", javaCallIO},
		{"org.apache.kafka.clients.admin.AdminClient", "close", javaCallUnknown},
		{"org.springframework.data.redis.connection.RedisConnection", "del", javaCallIO},
		{"java.nio.file.Files", "readString", javaCallIO},
		{"java.util.List", "get", javaCallPure},
		{"java.util.stream.Stream", "findFirst", javaCallPure},
		{"io.swagger.v3.oas.models.media.Schema", "getAllOf", javaCallPure},
		// These run or read what they were handed.
		{"java.util.function.Consumer", "accept", javaCallUnknown},
		{"java.util.concurrent.ExecutorService", "execute", javaCallUnknown},
		{"com.acme.Unlisted", "save", javaCallUnknown},
	}
	for _, tt := range tests {
		if got := javaLibraryCall(tt.typ, tt.method); got != tt.want {
			t.Errorf("javaLibraryCall(%s, %s) = %d, want %d", tt.typ, tt.method, got, tt.want)
		}
	}
}

func javaStrings(v any) []string {
	s, _ := v.([]string)
	return s
}

// A statement run through a JdbcTemplate field is I/O, and what calls the method
// that runs it reaches I/O. A getter of an in-memory model is not, by any name.
func TestJavaCallOnALibraryReceiverIsClassedByMember(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/sql/Partitions.java": `package com.acme.sql;

import io.swagger.v3.oas.models.media.Schema;
import java.util.List;
import org.springframework.jdbc.core.JdbcTemplate;

public class Partitions {
    private final JdbcTemplate jdbcTemplate;

    public Partitions(JdbcTemplate jdbcTemplate) {
        this.jdbcTemplate = jdbcTemplate;
    }

    boolean drop(String table, long ts) {
        jdbcTemplate.execute("DROP TABLE " + table + "_" + ts);
        return true;
    }

    public void dropBefore(String table, List<Long> partitions) {
        for (Long ts : partitions) {
            drop(table, ts);
        }
    }

    public void dropInline(List<String> tables) {
        for (String table : tables) {
            jdbcTemplate.update("DROP TABLE " + table);
        }
    }

    public int countAllOf(List<Schema> schemas) {
        int n = 0;
        for (Schema schema : schemas) {
            n += schema.getAllOf().size();
        }
        return n;
    }
}
`,
	})

	drop := javaSym(t, ff, "Partitions.drop")
	if got := javaStrings(drop.PropAny("io_calls")); !slices.Contains(got, "jdbcTemplate.execute") {
		t.Errorf("drop io_calls = %v, want jdbcTemplate.execute", got)
	}
	if !javaIO(javaSym(t, ff, "Partitions.dropBefore")) {
		t.Error("dropBefore reaches the statement through drop and must be performs_io")
	}
	inline := javaSym(t, ff, "Partitions.dropInline")
	if got := javaStrings(inline.PropAny("io_calls")); !slices.Contains(got, "jdbcTemplate.update") {
		t.Errorf("dropInline io_calls = %v, want jdbcTemplate.update", got)
	}

	count := javaSym(t, ff, "Partitions.countAllOf")
	if got := javaStrings(count.PropAny("pure_calls")); !slices.Contains(got, "schema.getAllOf") {
		t.Errorf("countAllOf pure_calls = %v, want schema.getAllOf", got)
	}
	if javaIO(count) {
		t.Error("countAllOf reads a model and must not be performs_io")
	}
	if got := javaStrings(drop.PropAny("pure_calls")); len(got) != 0 {
		t.Errorf("pure_calls is recorded for in-loop calls only; drop has %v", got)
	}
}

// Overloads share a name, and I/O travels by name. One overload that reads a
// file must not make the callers of the one that parses a string reach I/O.
func TestJavaLibraryIODoesNotMarkAnOverloadedName(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/util/Json.java": `package com.acme.util;

import java.nio.file.Files;
import java.nio.file.Path;

public class Json {
    public static String parse(String value) {
        return value.trim();
    }

    public static String parse(Path file) throws Exception {
        return Files.readString(file);
    }

    public static String load(Path file) throws Exception {
        return Files.readString(file);
    }
}
`,
		"src/main/java/com/acme/util/Mapper.java": `package com.acme.util;

import java.nio.file.Path;
import java.util.List;

public class Mapper {
    public int parseAll(List<String> values) {
        int n = 0;
        for (String v : values) {
            n += Json.parse(v).length();
        }
        return n;
    }

    public int loadAll(List<Path> files) throws Exception {
        int n = 0;
        for (Path f : files) {
            n += Json.load(f).length();
        }
        return n;
    }
}
`,
	})
	if javaIO(javaSym(t, ff, "Mapper.parseAll")) {
		t.Error("parseAll calls the overload that parses a string and must not be performs_io")
	}
	if !javaIO(javaSym(t, ff, "Mapper.loadAll")) {
		t.Error("loadAll calls a method with one declaration, which reads a file, and must be performs_io")
	}
	named := false
	for _, f := range ff {
		if f.Name == "com.acme.util.Json.parse" || len(f.Name) > 10 && f.Name[len(f.Name)-10:] == "Json.parse" {
			if slices.Contains(javaStrings(f.PropAny("io_calls")), "Files.readString") {
				named = true
			}
		}
	}
	if !named {
		t.Error("the overload that reads the file must still name the call in io_calls")
	}
}

// A client reached through a getter of the class is typed by what the getter is
// declared to return.
func TestJavaCallOnAGetterResultIsTypedByItsReturnType(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/sql/Repo.java": `package com.acme.sql;

import java.util.List;
import org.springframework.jdbc.core.JdbcTemplate;

public class Repo {
    private JdbcTemplate jdbcTemplate;

    protected JdbcTemplate getJdbcTemplate() {
        return jdbcTemplate;
    }

    Object pick(String a) { return a; }
    Object pick(int a) { return a; }

    public void dropAll(List<String> tables) {
        for (String table : tables) {
            getJdbcTemplate().execute("DROP TABLE " + table);
            this.getJdbcTemplate().update("VACUUM");
        }
    }
}
`,
	})
	drop := javaSym(t, ff, "Repo.dropAll")
	got := javaStrings(drop.PropAny("io_calls"))
	if !slices.Contains(got, "getJdbcTemplate().execute") || !slices.Contains(got, "this.getJdbcTemplate().update") {
		t.Errorf("dropAll io_calls = %v, want both calls on the getter's result", got)
	}
	if !javaIO(drop) {
		t.Error("dropAll runs a statement per table and must be performs_io")
	}
}

// An interface shared by repositories is no repository itself. A call through it
// still runs a query, whichever repository is behind it.
func TestJavaInterfaceSharedByRepositoriesIsAnIOType(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/event/EventRepository.java": `package com.acme.event;

public interface EventRepository<T> {
    void removeEvents(long before);
}
`,
		"src/main/java/com/acme/event/ErrorEventRepository.java": `package com.acme.event;

import org.springframework.data.jpa.repository.JpaRepository;

public interface ErrorEventRepository extends EventRepository<ErrorEvent>, JpaRepository<ErrorEvent, Long> {
}
`,
		"src/main/java/com/acme/event/Named.java": `package com.acme.event;

public interface Named {
    String label();
}
`,
		"src/main/java/com/acme/event/Tag.java": `package com.acme.event;

public class Tag implements Named {
    public String label() { return "tag"; }
}
`,
		"src/main/java/com/acme/event/EventDao.java": `package com.acme.event;

import java.util.List;

public class EventDao {
    private ErrorEventRepository errors;

    private EventRepository<?> repositoryFor(String type) {
        return errors;
    }

    public void removeAll(List<String> types, long before) {
        for (String type : types) {
            repositoryFor(type).removeEvents(before);
        }
    }

    public int labels(List<Named> items) {
        int n = 0;
        for (Named item : items) {
            n += item.label().length();
        }
        return n;
    }
}
`,
	})
	if !javaIO(javaSym(t, ff, "EventDao.removeAll")) {
		t.Errorf("removeAll calls a repository through the interface they share and must be performs_io; calls: %v",
			javaCallTargets(javaSym(t, ff, "EventDao.removeAll")))
	}
	if javaIO(javaSym(t, ff, "EventDao.labels")) {
		t.Error("an interface implemented by an ordinary class is no io_type")
	}
}
