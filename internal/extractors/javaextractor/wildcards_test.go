package javaextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A type imported on demand (`import com.acme.mapper.*;`) is the receiver type of
// the calls on a field declared with it, the target of a `new`, and a supertype.
// The file's own package is looked in first and the on-demand packages after it,
// as javac does.
func TestJavaWildcardImportedTypesResolveReceiversAndTargets(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/mapper/SkuMapper.java": `package com.acme.mapper;

public interface SkuMapper {
    Sku selectByPrimaryKey(Long id);
    int updateByPrimaryKeySelective(Sku sku);
}
`,
		"src/main/java/com/acme/model/Sku.java": `package com.acme.model;

public class Sku {
}
`,
		"src/main/java/com/acme/model/SkuExample.java": `package com.acme.model;

public class SkuExample {
}
`,
		"src/main/java/com/acme/model/BaseService.java": `package com.acme.model;

public class BaseService {
}
`,
		"src/main/java/com/acme/svc/impl/Audit.java": `package com.acme.svc.impl;

public class Audit {
    public void record(String what) {}
}
`,
		"src/main/java/com/acme/other/Audit.java": `package com.acme.other;

public class Audit {
    public void record(String what) {}
}
`,
		"src/main/java/com/acme/svc/impl/StockServiceImpl.java": `package com.acme.svc.impl;

import com.acme.mapper.*;
import com.acme.model.*;
import com.acme.other.*;
import java.util.List;

public class StockServiceImpl extends BaseService {
    private SkuMapper skuMapper;
    private Audit audit;

    public void lockStock(List<Long> ids) {
        for (Long id : ids) {
            Sku sku = skuMapper.selectByPrimaryKey(id);
            skuMapper.updateByPrimaryKeySelective(sku);
        }
        audit.record("lock");
    }

    public SkuExample example() {
        return new SkuExample();
    }
}
`,
	})

	lock := javaSym(t, ff, "StockServiceImpl.lockStock")
	calls := javaCallTargets(lock)
	for _, want := range []string{
		"src/main/java/com/acme/mapper.SkuMapper.selectByPrimaryKey",
		"src/main/java/com/acme/mapper.SkuMapper.updateByPrimaryKeySelective",
		// Own package before an on-demand import that declares the same name.
		"src/main/java/com/acme/svc/impl.Audit.record",
	} {
		if !slices.Contains(calls, want) {
			t.Errorf("lockStock calls %v, want %s", calls, want)
		}
	}
	if slices.Contains(calls, "src/main/java/com/acme/other.Audit.record") {
		t.Errorf("lockStock calls the on-demand Audit, which the own package's shadows: %v", calls)
	}

	ex := javaSym(t, ff, "StockServiceImpl.example")
	if !hasRel(ex.Relations, facts.RelInstantiates, "src/main/java/com/acme/model.SkuExample") {
		t.Errorf("example relations %v, want instantiates model.SkuExample", ex.Relations)
	}

	cls := javaSym(t, ff, "impl.StockServiceImpl")
	if !hasRel(cls.Relations, facts.RelImplements, "src/main/java/com/acme/model.BaseService") {
		t.Errorf("class relations %v, want implements model.BaseService", cls.Relations)
	}
	ft, _ := cls.PropAny(facts.PropFieldTypes).(map[string]string)
	if got := ft["skuMapper"]; got != "com.acme.mapper.SkuMapper" {
		t.Errorf("field_types[skuMapper] = %q, want com.acme.mapper.SkuMapper", got)
	}
}

// A name no candidate of which the repository declares keeps the own-package
// reading it had: a library type imported on demand is not guessed at.
func TestJavaWildcardUndeclaredTypeKeepsOwnPackageTarget(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/main/java/com/acme/svc/Maker.java": `package com.acme.svc;

import org.lib.*;

public class Maker {
    public Object make() {
        return new Widget();
    }
}
`,
	})
	mk := javaSym(t, ff, "Maker.make")
	if !hasRel(mk.Relations, facts.RelInstantiates, "com.acme.svc.Widget") {
		t.Errorf("make relations %v, want instantiates com.acme.svc.Widget", mk.Relations)
	}
}
