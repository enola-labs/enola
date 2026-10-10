package kotlinextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractKotlinRepo(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	var rel []string
	for name, src := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, name)
	}
	slices.Sort(rel)
	ff, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func ktSym(t *testing.T, ff []facts.Fact, suffix string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			return f
		}
	}
	var names []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			names = append(names, f.Name)
		}
	}
	t.Fatalf("no symbol ending in %s among %v", suffix, names)
	return facts.Fact{}
}

func ktRelSuffix(f facts.Fact, kind, suffix string) bool {
	return slices.ContainsFunc(f.Relations, func(r facts.Relation) bool {
		return r.Kind == kind && strings.Contains(r.Target, "/") && strings.HasSuffix(r.Target, suffix)
	})
}

func ktIO(f facts.Fact) bool {
	b, _ := f.PropAny("performs_io").(bool)
	return b
}

const ktDaoFixture = `package com.acme.data

import androidx.room.Dao
import androidx.room.Insert

@Dao
interface TopicDao {
    @Insert
    fun insert(topic: Topic)

    fun label(topic: Topic): String = topic.name
}
`

// The chain an Android app is built from: a view model holds a repository
// interface, its implementation holds a DAO, and the DAO method is the round trip.
func TestKotlinCallOnADeclaredReceiverIsAnEdge(t *testing.T) {
	ff := extractKotlinRepo(t, map[string]string{
		"app/src/main/kotlin/com/acme/data/TopicDao.kt": ktDaoFixture,
		"app/src/main/kotlin/com/acme/data/TopicsRepository.kt": `package com.acme.data

interface TopicsRepository {
    fun save(topic: Topic)
    fun title(topic: Topic): String
}
`,
		"app/src/main/kotlin/com/acme/data/OfflineTopicsRepository.kt": `package com.acme.data

class OfflineTopicsRepository(private val dao: TopicDao) : TopicsRepository {
    override fun save(topic: Topic) {
        dao.insert(topic)
    }

    override fun title(topic: Topic): String = dao.label(topic)
}
`,
		"app/src/main/kotlin/com/acme/ui/TopicsViewModel.kt": `package com.acme.ui

import com.acme.data.TopicsRepository

class TopicsViewModel(private val repository: TopicsRepository) {
    fun saveAll(topics: List<Topic>) {
        for (topic in topics) {
            repository.save(topic)
        }
    }

    fun titles(topics: List<Topic>) {
        for (topic in topics) {
            repository.title(topic)
        }
    }
}
`,
	})
	impl := ktSym(t, ff, "OfflineTopicsRepository.save")
	if !ktRelSuffix(impl, facts.RelCalls, "data.TopicDao.insert") {
		t.Errorf("OfflineTopicsRepository.save relations = %v, want a call to the DAO's insert", impl.Relations)
	}
	// The supertype, written by its simple name, is an edge to the interface.
	if repo := ktSym(t, ff, "data.OfflineTopicsRepository"); !ktRelSuffix(repo, facts.RelImplements, "data.TopicsRepository") {
		t.Errorf("OfflineTopicsRepository relations = %v, want implements the interface by its canonical name", repo.Relations)
	}
	all := ktSym(t, ff, "TopicsViewModel.saveAll")
	if !ktRelSuffix(all, facts.RelCalls, "data.TopicsRepository.save") {
		t.Errorf("saveAll relations = %v, want a call to the repository interface's save", all.Relations)
	}
	if inLoop, _ := all.PropAny("calls_in_loop").([]string); len(inLoop) != 1 || !strings.HasSuffix(inLoop[0], "data.TopicsRepository.save") {
		t.Errorf("saveAll calls_in_loop = %v, want the resolved method", inLoop)
	}
	if !ktIO(all) {
		t.Errorf("saveAll is not flagged: view model, interface, implementation, DAO")
	}
	if ktIO(ktSym(t, ff, "TopicsViewModel.titles")) {
		t.Errorf("titles is flagged: its chain ends at a property read")
	}
	for _, f := range ff {
		for _, leak := range []string{"typed_calls", "super_candidates"} {
			if _, kept := f.Prop(leak); kept {
				t.Errorf("%s: the walker's note %s leaked into the facts", f.Name, leak)
			}
		}
	}
}

// A property, a parameter, a typed local and a constructed local each state the
// receiver's type. A local assigned from a call does not, and a type from outside
// the repository names nothing here.
func TestKotlinDeclaredReceiverForms(t *testing.T) {
	ff := extractKotlinRepo(t, map[string]string{
		"app/src/main/kotlin/com/acme/data/TopicDao.kt": ktDaoFixture,
		"app/src/main/kotlin/com/acme/data/Cache.kt": `package com.acme.data

class Cache {
    fun put(topic: Topic) {}
}
`,
		"app/src/main/kotlin/com/acme/ui/Forms.kt": `package com.acme.ui

import com.acme.data.Cache
import com.acme.data.TopicDao
import java.util.ArrayList

class Forms {
    lateinit var dao: TopicDao

    fun byProperty(topic: Topic) { dao.insert(topic) }

    fun byParameter(other: TopicDao, topic: Topic) { other.insert(topic) }

    fun byTypedLocal(topic: Topic) {
        val local: TopicDao = find()
        local.insert(topic)
    }

    fun byConstruction(topic: Topic) {
        val cache = Cache()
        cache.put(topic)
    }

    fun unknown(topic: Topic) {
        val found = find()
        found.insert(topic)
    }

    fun foreign(topic: Topic) {
        val list = ArrayList<Topic>()
        list.add(topic)
    }
}
`,
	})
	for suffix, want := range map[string]string{
		"Forms.byProperty":     "data.TopicDao.insert",
		"Forms.byParameter":    "data.TopicDao.insert",
		"Forms.byTypedLocal":   "data.TopicDao.insert",
		"Forms.byConstruction": "data.Cache.put",
	} {
		if f := ktSym(t, ff, suffix); !ktRelSuffix(f, facts.RelCalls, want) {
			t.Errorf("%s relations = %v, want a call to %s", suffix, f.Relations, want)
		}
	}
	for suffix, method := range map[string]string{"Forms.unknown": ".insert", "Forms.foreign": ".add"} {
		if f := ktSym(t, ff, suffix); ktRelSuffix(f, facts.RelCalls, method) {
			t.Errorf("%s has a resolved edge to %s, and its receiver's type is not declared here: %v", suffix, method, f.Relations)
		}
	}
}
