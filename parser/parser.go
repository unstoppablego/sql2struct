package parser

import (
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"text/template"

	"github.com/blastrain/vitess-sqlparser/tidbparser/ast"
	"github.com/blastrain/vitess-sqlparser/tidbparser/dependency/mysql"
	"github.com/blastrain/vitess-sqlparser/tidbparser/dependency/types"
	"github.com/blastrain/vitess-sqlparser/tidbparser/parser"
	"github.com/jinzhu/inflection"
	"github.com/pkg/errors"
)

var (
	structTmplRaw   string
	fileTmplRaw     string
	enumTmplRaw     string
	FindByIdTmplRaw string
	structTmpl      *template.Template
	fileTmpl        *template.Template
	enumTmpl        *template.Template
	FindPkTmpl      *template.Template
	tmplParseOnce   sync.Once
)

var acronym = map[string]struct{}{
	"ID":  {},
	"IP":  {},
	"RPC": {},
}

type ModelCodes struct {
	Package    string
	ImportPath []string
	StructCode []string
	EmunData   []string
	Pkcode     []string
}

type FindByPkCodes struct {
	TstructName string
	PkName      string
}

func ParseSql(sql string, options ...Option) (ModelCodes, error) {
	initTemplate()
	opt := parseOption(options)

	stmts, err := parser.New().Parse(sql, opt.Charset, opt.Collation)
	if err != nil {
		return ModelCodes{}, err
	}
	tableStr := make([]string, 0, len(stmts))
	importPath := make(map[string]struct{})
	var emunData []string
	var pkCodeSlice []string
	for _, stmt := range stmts {
		if ct, ok := stmt.(*ast.CreateTableStmt); ok {
			pkcode, emun, s, ipt, err := makeCode(ct, opt)
			if err != nil {
				return ModelCodes{}, err
			}
			emunData = append(emunData, emun...)
			pkCodeSlice = append(pkCodeSlice, pkcode...)
			tableStr = append(tableStr, s)
			for _, s := range ipt {
				importPath[s] = struct{}{}
			}
		}
	}
	importPathArr := make([]string, 0, len(importPath))
	for s := range importPath {
		importPathArr = append(importPathArr, s)
	}

	if len(pkCodeSlice) > 0 {
		importPathArr = append(importPathArr, `gorm.io/gorm`)
	}

	sort.Strings(importPathArr)
	return ModelCodes{
		Package:    opt.Package,
		ImportPath: importPathArr,
		StructCode: tableStr,
		EmunData:   emunData,
		Pkcode:     pkCodeSlice,
	}, nil
}

func ParseSqlToWrite(sql string, writer io.Writer, options ...Option) error {
	data, err := ParseSql(sql, options...)
	if err != nil {
		return err
	}
	err = fileTmpl.Execute(writer, data)
	if err != nil {
		return err
	}
	return nil
}

func ConfigureAcronym(words []string) {
	acronym = make(map[string]struct{}, len(words))
	for _, w := range words {
		acronym[strings.ToUpper(w)] = struct{}{}
	}
}

type tmplData struct {
	TableName    string
	NameFunc     bool
	RawTableName string
	Fields       []tmplField
	Comment      string
}

type tmplField struct {
	Name      string
	GoType    string
	Tag       string
	Comment   string
	EmunName  string //
	EmunValue []string
}

func makeCode(stmt *ast.CreateTableStmt, opt options) ([]string, []string, string, []string, error) {
	importPath := make([]string, 0, 1)
	var emumData []string
	var pkCode []string

	data := tmplData{
		TableName:    stmt.Table.Name.String(),
		RawTableName: stmt.Table.Name.String(),
		Fields:       make([]tmplField, 0, 1),
	}
	tablePrefix := opt.TablePrefix
	if tablePrefix != "" && strings.HasPrefix(data.TableName, tablePrefix) {
		data.NameFunc = true
		data.TableName = data.TableName[len(tablePrefix):]
	}
	if opt.ForceTableName || data.RawTableName != inflection.Plural(data.RawTableName) {
		data.NameFunc = true
	}

	data.TableName = toCamel(data.TableName)

	// find table comment
	for _, opt := range stmt.Options {
		if opt.Tp == ast.TableOptionComment {
			data.Comment = opt.StrValue
			break
		}
	}

	isPrimaryKey := make(map[string]bool)
	for _, con := range stmt.Constraints {
		if con.Tp == ast.ConstraintPrimaryKey {
			isPrimaryKey[con.Keys[0].Column.String()] = true
		}
	}

	columnPrefix := opt.ColumnPrefix
	for _, col := range stmt.Cols {
		colName := col.Name.Name.String()
		goFieldName := colName
		if columnPrefix != "" && strings.HasPrefix(goFieldName, columnPrefix) {
			goFieldName = goFieldName[len(columnPrefix):]
		}

		field := tmplField{
			Name: toCamel(goFieldName),
		}

		tags := make([]string, 0, 4)
		// make GORM's tag
		gormTag := strings.Builder{}
		gormTag.WriteString("column:")
		gormTag.WriteString(colName)

		//在这里解析出type 数据内容
		if opt.GormType {
			gormTag.WriteString(";type:")
			gormTag.WriteString(col.Tp.InfoSchemaStr())
		}

		if isPrimaryKey[colName] {
			gormTag.WriteString(";primary_key")

		}
		isNotNull := false
		canNull := false
		for _, o := range col.Options {
			switch o.Tp {
			case ast.ColumnOptionPrimaryKey:
				if !isPrimaryKey[colName] {
					gormTag.WriteString(";primary_key")
					isPrimaryKey[colName] = true
				}
			case ast.ColumnOptionNotNull:
				isNotNull = true
			case ast.ColumnOptionAutoIncrement:
				gormTag.WriteString(";AUTO_INCREMENT")
			case ast.ColumnOptionDefaultValue:
				if value := getDefaultValue(o.Expr); value != "" {
					gormTag.WriteString(";default:")
					gormTag.WriteString(value)
				}
			case ast.ColumnOptionUniqKey:
				gormTag.WriteString(";unique")
			case ast.ColumnOptionNull:
				//gormTag.WriteString(";NULL")
				canNull = true
			case ast.ColumnOptionOnUpdate: // For Timestamp and Datetime only.
			case ast.ColumnOptionFulltext:
			case ast.ColumnOptionComment:
				field.Comment = o.Expr.GetDatum().GetString()
			default:
				//return "", nil, errors.Errorf(" unsupport option %d\n", o.Tp)
			}
		}
		if !isPrimaryKey[colName] && isNotNull {
			gormTag.WriteString(";NOT NULL")
		}
		tags = append(tags, "gorm", gormTag.String())

		if opt.JsonTag {
			tags = append(tags, "json", colName)
		}

		field.Tag = makeTagStr(tags)

		// get type in golang
		nullStyle := opt.NullStyle
		if !canNull {
			nullStyle = NullDisable
		}
		goType, pkg := mysqlToGoType(col.Tp, nullStyle)
		if pkg != "" {
			importPath = append(importPath, pkg)

		}
		//-----------
		if goType == "emun" {

			field.EmunName = data.TableName + "_" + toCamel(goFieldName)
			emunContent := col.Tp.InfoSchemaStr()
			emunContent = strings.Replace(emunContent, "enum(", "[", 1)
			emunContent = strings.Replace(emunContent, "')", "']", 1)
			emunContent = strings.Replace(emunContent, "'", "\"", -1)
			var emunValue []string
			err := json.Unmarshal([]byte(emunContent), &emunValue)
			if err != nil {
				log.Panic(err)
			}

			field.EmunValue = emunValue

			field.GoType = field.EmunName

			builder := strings.Builder{}
			err = enumTmpl.Execute(&builder, field)
			if err != nil {
				log.Panic(err)
			}
			emumData = append(emumData, builder.String())
		} else {
			field.GoType = goType
		}

		if x, ok := isPrimaryKey[colName]; ok {
			if x {
				var a FindByPkCodes
				a.PkName = colName
				a.TstructName = data.TableName
				builder := strings.Builder{}
				err := FindPkTmpl.Execute(&builder, a)
				if err != nil {
					log.Panic(err)
				}
				pkCode = append(pkCode, builder.String())
			}
		}
		data.Fields = append(data.Fields, field)
	}

	builder := strings.Builder{}
	err := structTmpl.Execute(&builder, data)
	if err != nil {
		return pkCode, emumData, "", nil, err
	}

	code, err := format.Source([]byte(builder.String()))
	if err != nil {
		return pkCode, emumData, string(code), importPath, errors.WithMessage(err, "format golang code error")
	}
	return pkCode, emumData, string(code), importPath, nil
}

func mysqlToGoType(colTp *types.FieldType, style NullStyle) (name string, path string) {
	if style == NullInSql {
		path = "database/sql"
		switch colTp.Tp {
		case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong:
			name = "sql.NullInt32"
		case mysql.TypeLonglong:
			name = "sql.NullInt64"
		case mysql.TypeFloat, mysql.TypeDouble:
			name = "sql.NullFloat64"
		case mysql.TypeString, mysql.TypeVarchar, mysql.TypeVarString,
			mysql.TypeBlob, mysql.TypeTinyBlob, mysql.TypeMediumBlob, mysql.TypeLongBlob:
			name = "sql.NullString"
		case mysql.TypeTimestamp, mysql.TypeDatetime, mysql.TypeDate:
			name = "sql.NullTime"
		case mysql.TypeDecimal, mysql.TypeNewDecimal:
			name = "sql.NullString"
		case mysql.TypeJSON:
			name = "sql.NullString"
		default:
			return "UnSupport", ""
		}
	} else {
		switch colTp.Tp {
		case mysql.TypeTiny, mysql.TypeShort, mysql.TypeInt24, mysql.TypeLong:
			if mysql.HasUnsignedFlag(colTp.Flag) {
				name = "uint"
			} else {
				name = "int"
			}
		case mysql.TypeLonglong:
			if mysql.HasUnsignedFlag(colTp.Flag) {
				name = "uint64"
			} else {
				name = "int64"
			}
		case mysql.TypeFloat, mysql.TypeDouble:
			name = "float64"
		case mysql.TypeString, mysql.TypeVarchar, mysql.TypeVarString,
			mysql.TypeBlob, mysql.TypeTinyBlob, mysql.TypeMediumBlob, mysql.TypeLongBlob:
			name = "string"
		case mysql.TypeTimestamp, mysql.TypeDatetime, mysql.TypeDate:
			path = "time"
			name = "time.Time"
		case mysql.TypeDecimal, mysql.TypeNewDecimal:
			name = "decimal.Decimal"
			path = "github.com/shopspring/decimal"
		case mysql.TypeJSON:
			name = "string"
		case mysql.TypeEnum:
			return "emun", ""
		default:
			return "UnSupport", ""
		}
		if style == NullInPointer {
			name = "*" + name
		}
	}
	return
}

func makeTagStr(tags []string) string {
	builder := strings.Builder{}
	for i := 0; i < len(tags)/2; i++ {
		builder.WriteString(tags[i*2])
		builder.WriteString(`:"`)
		builder.WriteString(tags[i*2+1])
		builder.WriteString(`" `)
	}
	if builder.Len() > 0 {
		return builder.String()[:builder.Len()-1]
	}
	return builder.String()
}

func getDefaultValue(expr ast.ExprNode) (value string) {
	if expr.GetDatum().Kind() != types.KindNull {
		value = fmt.Sprintf("%v", expr.GetDatum().GetValue())
	} else if expr.GetFlag() != ast.FlagConstant {
		if expr.GetFlag() == ast.FlagHasFunc {
			if funcExpr, ok := expr.(*ast.FuncCallExpr); ok {
				value = funcExpr.FnName.O
			}
		}
	}
	return
}

func toCamel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	s += "."

	n := strings.Builder{}
	n.Grow(len(s))
	temp := strings.Builder{}
	temp.Grow(len(s))
	wordFirst := true
	for _, v := range []byte(s) {
		vIsCap := v >= 'A' && v <= 'Z'
		vIsLow := v >= 'a' && v <= 'z'
		if wordFirst && vIsLow {
			v -= 'a' - 'A'
		}

		if vIsCap || vIsLow {
			temp.WriteByte(v)
			wordFirst = false
		} else {
			isNum := v >= '0' && v <= '9'
			wordFirst = isNum || v == '_' || v == ' ' || v == '-' || v == '.'
			if temp.Len() > 0 && wordFirst {
				word := temp.String()
				upper := strings.ToUpper(word)
				if _, ok := acronym[upper]; ok {
					n.WriteString(upper)
				} else {
					n.WriteString(word)
				}
				temp.Reset()
			}
			if isNum {
				n.WriteByte(v)
			}
		}
	}
	return n.String()
}

func initTemplate() {
	tmplParseOnce.Do(func() {
		var err error
		structTmpl, err = template.New("goStruct").Parse(structTmplRaw)
		if err != nil {
			panic(err)
		}

		fileTmpl, err = template.New("goFile").Parse(fileTmplRaw)
		if err != nil {
			panic(err)
		}

		enumTmpl, err = template.New("emunTmplRaw").Parse(enumTmplRaw)
		if err != nil {
			panic(err)
		}

		FindPkTmpl, err = template.New("FindByIdTmplRaw").Parse(FindByIdTmplRaw)
		if err != nil {
			panic(err)
		}
	})
}

func init() {

	FindByIdTmplRaw = `
func Get{{.TstructName}}PK_{{.PkName}}(id int, tx *gorm.DB) *{{.TstructName}} {

	var a {{.TstructName}}
	if err := tx.Where("{{.PkName}} = ?", id).Take(&a).Error; err == nil {
		return &a
	}
	return nil
}
`

	enumTmplRaw = `type {{.EmunName}} string
{{range .EmunValue}}
const {{$.EmunName}}_{{.}} {{$.EmunName}} = "{{.}}"
{{end}}
var {{.EmunName}}_Slice = []string{
{{range .EmunValue}}
string({{$.EmunName}}_{{.}}),
{{end}}
}
`

	structTmplRaw = `
{{- if .Comment -}}
// {{.Comment}}
{{end -}}
type {{.TableName}} struct {
{{- range .Fields}}
	{{.Name}} {{.GoType}} {{if .Tag}}` + "`{{.Tag}}`" + `{{end}}{{if .Comment}} // {{.Comment}}{{end}}
{{- end}}
}
{{if .NameFunc}}
func (m *{{.TableName}}) TableName() string {
	return "{{.RawTableName}}"
}
{{end}}`

	fileTmplRaw = `// Code generated by github.com/unstoppablego/sql2struct
package {{.Package}}
{{if .ImportPath}}
import (
	{{- range .ImportPath}}
	"{{.}}"
	{{- end}}
)
{{- end}}

{{if .EmunData}}
{{- range .EmunData}}
{{.}}
{{- end}}
{{- end}}

{{range .StructCode}}
{{.}}
{{end}}

{{if .Pkcode}}
{{- range .Pkcode}}
{{.}}
{{- end}}
{{- end}}
`
}
