package javaextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// MyBatis mappers are interfaces whose every abstract method is a statement the
// framework runs. Spring registers them by a @MapperScan package or by MyBatis's
// own @Mapper; neither is a Spring Data repository, so they carried no I/O and a
// mapper call in a loop read as an ordinary call.

const mybatisScanConfig = `package app.config;

import org.mybatis.spring.annotation.MapperScan;
import org.springframework.context.annotation.Configuration;

@Configuration
@MapperScan({"app.mapper", "app.dao"})
public class MyBatisConfig {
}
`

func TestJavaIO_MapperScanPackageInterfaceIsIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/config/MyBatisConfig.java": mybatisScanConfig,
		"app/mapper/StockMapper.java": `package app.mapper;

public interface StockMapper {
    Stock selectByPrimaryKey(Long id);
    int updateByPrimaryKeySelective(Stock row);
    default int touch(Stock row) { return 0; }
}
`,
		"app/mapper/sub/ItemMapper.java": `package app.mapper.sub;

public interface ItemMapper {
    int insert(Item row);
}
`,
		"app/mapper/StockMapperHelper.java": `package app.mapper;

public class StockMapperHelper {
    public int count(Stock row) { return 1; }
}
`,
		"app/other/OtherApi.java": `package app.other;

public interface OtherApi {
    int insert(Object row);
}
`,
		"app/service/OrderService.java": `package app.service;

import app.mapper.StockMapper;
import java.util.List;

public class OrderService {
    private StockMapper stockMapper;

    public void lockStock(List<Long> ids) {
        for (Long id : ids) {
            Stock s = stockMapper.selectByPrimaryKey(id);
            stockMapper.updateByPrimaryKeySelective(s);
        }
    }
}
`,
	})
	assertPerformsIO(t, ff, "app/mapper.StockMapper.selectByPrimaryKey")
	assertPerformsIO(t, ff, "app/mapper.StockMapper.updateByPrimaryKeySelective")
	assertPerformsIO(t, ff, "app/mapper/sub.ItemMapper.insert") // scanning is recursive
	assertNotPerformsIO(t, ff, "app/mapper.StockMapper.touch")  // a default method runs no statement
	assertNotPerformsIO(t, ff, "app/mapper.StockMapperHelper.count")
	assertNotPerformsIO(t, ff, "app/other.OtherApi.insert")

	f, _ := findFact(ff, "app/service.OrderService.lockStock")
	if v, _ := f.Props["performs_io"].(bool); !v {
		t.Errorf("lockStock performs_io = %v, want true (it calls mapper methods); props=%v", f.Props["performs_io"], f.Props)
	}
	if v, _ := mustFact(t, ff, "app/mapper.StockMapper").Props[propIOType].(bool); !v {
		t.Errorf("StockMapper should be an io_type")
	}
	assertNoMapperScratch(t, ff)
}

// MyBatis's own @Mapper marks the interface wherever it sits.
func TestJavaIO_MyBatisMapperAnnotationIsIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/dao/UserDao.java": `package app.dao;

import org.apache.ibatis.annotations.Mapper;
import org.apache.ibatis.annotations.Select;

@Mapper
public interface UserDao {
    @Select("select * from users where id = #{id}")
    User find(long id);
}
`,
	})
	assertPerformsIO(t, ff, "app/dao.UserDao.find")
	assertNoMapperScratch(t, ff)
}

// MapStruct's @Mapper generates a bean converter: no statement runs. Only the
// import tells the two apart.
func TestJavaIO_MapStructMapperIsNotIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/convert/UserConverter.java": `package app.convert;

import org.mapstruct.Mapper;

@Mapper(componentModel = "spring")
public interface UserConverter {
    UserDto toDto(User user);
}
`,
		"app/config/Scan.java": `package app.config;

import other.MapperScan;

@MapperScan("app.convert")
public class Scan {
}
`,
	})
	assertNotPerformsIO(t, ff, "app/convert.UserConverter.toDto")
	assertNoMapperScratch(t, ff)
}

// A mapper extending a library base (MyBatis-Plus BaseMapper) inherits methods
// no declaration in the repository names: a call to one is still a round trip.
func TestJavaIO_ScannedMapperInheritedCallIsIO(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/config/MyBatisConfig.java": `package app.config;

import org.mybatis.spring.annotation.MapperScan;

@MapperScan(basePackages = "app.mapper")
public class MyBatisConfig {
}
`,
		"app/mapper/UserMapper.java": `package app.mapper;

import com.baomidou.mybatisplus.core.mapper.BaseMapper;

public interface UserMapper extends BaseMapper<User> {
}
`,
		"app/service/UserService.java": `package app.service;

import app.mapper.UserMapper;

public class UserService {
    private UserMapper userMapper;

    public User get(long id) {
        return userMapper.selectById(id);
    }
}
`,
	})
	assertPerformsIO(t, ff, "app/service.UserService.get")
}

func mustFact(t *testing.T, ff []facts.Fact, name string) facts.Fact {
	t.Helper()
	f, ok := findFact(ff, name)
	if !ok {
		t.Fatalf("missing %q", name)
	}
	return f
}

func assertNoMapperScratch(t *testing.T, ff []facts.Fact) {
	t.Helper()
	for _, f := range ff {
		for _, p := range []string{propMapperScan, propMyBatisMapper, propAbstractDecl} {
			if _, ok := f.Props[p]; ok {
				t.Errorf("%s still carries the transient prop %s", f.Name, p)
			}
		}
	}
}
