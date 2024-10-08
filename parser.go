/*
Copyright 2021 Lee R. Boynton

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package smithy

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/boynton/data"
)

const GenericAccept = "*"

var AnnotateSources bool = false

func Parse(path string, opts ...ParserOption) (*AST, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src := string(b)

	return parse(path, src, opts...)
}

func ParseString(s string, opts ...ParserOption) (*AST, error) {
	return parse(":memory:", s, opts...)
}

func parse(path string, content string, opts ...ParserOption) (*AST, error) {
	p := &Parser{
		scanner:  NewScanner(strings.NewReader(content)),
		path:     path,
		source:   content,
		visitors: map[string]TraitVisitor{},
	}

	p.addVisitors(DefaultTraitVisitors()...)

	for _, opt := range opts {
		opt(p)
	}

	p.wd, _ = os.Getwd()
	if err := p.Parse(); err != nil {
		return nil, err
	}
	return p.ast, nil
}

type ParserOption func(*Parser)

type Parser struct {
	path           string
	source         string
	scanner        *Scanner
	ast            *AST
	lastToken      *Token
	prevLastToken  *Token
	ungottenToken  *Token
	namespace      string
	name           string
	currentComment string
	use            map[string]string //maps short name to fully qualified name (typically another namespace)
	wd             string
	version        int //1 or 2
	visitors       map[string]TraitVisitor
}

type TraitVisitor interface {
	Accepts() []string
	Parse(p ASTParser, name string, traits *data.Object) (*data.Object, error)
}

type ASTParser interface {
	EnsureNamespaced(name string) string
	Expect(tokenType TokenType) error
	ExpectIdentifier() (string, error)
	ExpectNumber() (*data.Decimal, error)
	ExpectInt() (int, error)
	ExpectString() (string, error)
	ExpectStringArray() ([]string, error)
	ExpectIdentifierArray() ([]string, error)
	ExpectIdentifierMap() (map[string]string, error)
	GetToken() *Token
	MergeComment(comment1 string, comment2 string) string
	ParseTraitArgs() (*data.Object, interface{}, error)
	WithTrait(traits *data.Object, key string, val interface{}) *data.Object
	WithCommentTrait(traits *data.Object, namespace string, val string) (*data.Object, string)
	Error(msg string) error
	SyntaxError() error
	Warning(msg string)
	EndOfFileError() error
}

func WithTraitVisitors(visitors ...TraitVisitor) ParserOption {
	return func(p *Parser) {
		p.addVisitors(visitors...)
	}
}

func (p *Parser) addVisitors(visitors ...TraitVisitor) {
	for _, v := range visitors {
		for _, ac := range v.Accepts() {
			p.visitors[ac] = v
		}
	}
}

func (p *Parser) Parse() error {
	var comment string
	var traits *data.Object
	p.ast = &AST{
		Smithy: "2",
	}
	for {
		var err error
		tok := p.GetToken()
		if tok == nil {
			break
		}
		switch tok.Type {
		case SYMBOL:
			switch tok.Text {
			case "namespace":
				if traits != nil {
					return p.SyntaxError()
				}
				err = p.parseNamespace(comment)
			case "metadata":
				if traits != nil {
					return p.SyntaxError()
				}
				err = p.parseMetadata()
			case "service":
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseService(traits)
			case "blob", "document":
				err = p.Error(fmt.Sprintf("Shape NYI: %s", tok.Text))
			case "byte", "short", "integer", "long", "float", "double", "bigInteger", "bigDecimal", "string", "timestamp", "boolean":
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseSimpleTypeDef(tok.Text, traits)
				traits = nil
			case "enum", "intEnum":
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseEnum(traits, tok.Text == "intEnum")
				traits = nil
			case "structure":
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseStructure(traits)
				traits = nil
			case "union":
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseUnion(traits)
				traits = nil
			case "set":
				p.Warning("Deprecated shape: set")
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseList(traits)
				traits = nil
			case "list":
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseList(traits)
				traits = nil
			case "map":
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseMap(tok.Text, traits)
				traits = nil
			case "operation":
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseOperation(traits)
				traits = nil
			case "resource":
				traits, comment = withCommentTrait(traits, "", comment)
				err = p.parseResource(traits)
				traits = nil
			case "use":
				use, err := p.expectShapeId()
				if err == nil {
					shortName := StripNamespace(use)
					if p.use == nil {
						p.use = make(map[string]string, 0)
					}
					p.use[shortName] = use
				}
			case "apply":
				//to do: parse straight to a "target" shape, then apply it later during assembly?
				var ftype string
				ftype, err = p.expectShapeId()
				//ftype, err = p.expectTarget()
				tok := p.GetToken()
				if tok == nil {
					return p.SyntaxError()
				}
				if tok.Type != AT {
					return p.SyntaxError()
				}
				//to do: support apply on shape members
				if shape := p.ast.GetShape(p.ensureNamespaced(ftype)); shape != nil {
					t, e := p.parseTrait(shape.Traits)
					err = e
					shape.Traits = t
				}
			default:
				err = p.Error(fmt.Sprintf("Unknown shape: %s", tok.Text))
			}
			comment = ""
		case LINE_COMMENT:
			if strings.HasPrefix(tok.Text, "/") { //a triple slash means doc comment
				comment = p.MergeComment(comment, tok.Text[1:])
			}
		case AT:
			traits, err = p.parseTrait(traits)
		case DOLLAR:
			variable, err := p.ExpectIdentifier()
			if err != nil {
				return err
			}
			err = p.expect(COLON)
			if err != nil {
				return err
			}
			v, err := p.parseLiteralValue()
			if err != nil {
				return err
			}
			switch variable {
			case "version":
				if s, ok := v.(*string); ok {
					if strings.HasPrefix(*s, "1") {
						p.version = 1
					} else if strings.HasPrefix(*s, "2") {
						p.ast.Smithy = "2"
						p.version = 2
					} else {
						return fmt.Errorf("Unsupported version: %s\n", *s)
					}
				} else {
					return fmt.Errorf("Bad control statement (only version 1 or 1.0 is supported): $%s: %v\n", variable, v)
				}
			}
		case SEMICOLON, NEWLINE:
			/* ignore */
		default:
			return p.SyntaxError()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *Parser) UngetToken() {
	p.ungottenToken = p.lastToken
	p.lastToken = p.prevLastToken
}

func (p *Parser) GetToken() *Token {
	if p.ungottenToken != nil {
		p.lastToken = p.ungottenToken
		p.ungottenToken = nil
		return p.lastToken
	}
	p.prevLastToken = p.lastToken
	tok := p.scanner.Scan()
	for {
		if tok.Type == EOF {
			return nil //fixme
		} else if tok.Type != BLOCK_COMMENT {
			break
		}
		tok = p.scanner.Scan()
	}
	p.lastToken = &tok
	return p.lastToken
}

func (p *Parser) ignore(toktype TokenType) error {
	tok := p.GetToken()
	if tok == nil {
		return p.EndOfFileError()
	}
	if tok.Type == toktype {
		return nil
	}
	p.UngetToken()
	return nil
}

func (p *Parser) expect(toktype TokenType) error {
	tok := p.GetToken()
	if tok == nil {
		return p.EndOfFileError()
	}
	if tok.Type == toktype {
		return nil
	}
	return p.Error(fmt.Sprintf("Expected %v, found %v", toktype, tok.Type))
}

func (p *Parser) expectText() (string, error) {
	tok := p.GetToken()
	if tok == nil {
		return "", fmt.Errorf("Unexpected end of file")
	}
	if tok.IsText() {
		return tok.Text, nil
	}
	return "", fmt.Errorf("Expected symbol or string, found %v", tok.Type)
}

func (p *Parser) assertIdentifier(tok *Token) (string, error) {
	if tok == nil {
		return "", p.EndOfFileError()
	}
	if tok.Type == SYMBOL {
		return tok.Text, nil
	}
	return tok.Text, p.Error(fmt.Sprintf("Expected symbol, found %v", tok.Type))
}

func (p *Parser) ExpectIdentifier() (string, error) {
	tok := p.GetToken()
	return p.assertIdentifier(tok)
}

func (p *Parser) assertString(tok *Token) (string, error) {
	if tok == nil {
		return "", p.EndOfFileError()
	}
	if tok.Type == STRING {
		return tok.Text, nil
	}
	if tok.Type == UNDEFINED {
		return tok.Text, p.Error(tok.Text)
	}
	return tok.Text, p.Error(fmt.Sprintf("Expected string, found %v", tok.Type))
}

func (p *Parser) ExpectNumber() (*data.Decimal, error) {
	tok := p.GetToken()
	if tok == nil {
		return nil, p.EndOfFileError()
	}
	if tok.IsNumeric() {
		return data.ParseDecimal(tok.Text)
	}
	return nil, p.Error(fmt.Sprintf("Expected number, found %v", tok.Type))
}

func (p *Parser) ExpectInt() (int, error) {
	tok := p.GetToken()
	if tok == nil {
		return 0, p.EndOfFileError()
	}
	if tok.IsNumeric() {
		l, err := strconv.ParseInt(tok.Text, 10, 32)
		return int(l), err
	}
	return 0, p.Error(fmt.Sprintf("Expected integer, found %v", tok.Type))
}

func (p *Parser) ExpectString() (string, error) {
	tok := p.GetToken()
	return p.assertString(tok)
}

func (p *Parser) ExpectStringArray() ([]string, error) {
	tok := p.GetToken()
	if tok == nil {
		return nil, p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACKET {
		return nil, p.SyntaxError()
	}
	var items []string
	for {
		tok := p.GetToken()
		if tok == nil {
			return nil, p.EndOfFileError()
		}
		if tok.Type == CLOSE_BRACKET {
			break
		}
		s, err := p.assertString(tok)
		if err != nil {
			return nil, err
		}
		items = append(items, s)
		p.expect(COMMA)
	}
	return items, nil
}

func (p *Parser) ExpectIdentifierArray() ([]string, error) {
	tok := p.GetToken()
	if tok == nil {
		return nil, p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACKET {
		return nil, p.SyntaxError()
	}
	var items []string
	for {
		tok := p.GetToken()
		if tok == nil {
			return nil, p.EndOfFileError()
		}
		if tok.Type == CLOSE_BRACKET {
			break
		}
		if tok.Type == SYMBOL {
			items = append(items, tok.Text)
		} else if tok.Type == COMMA || tok.Type == NEWLINE || tok.Type == LINE_COMMENT {
			//ignore
		} else {
			return nil, p.SyntaxError()
		}
	}
	return items, nil
}

func (p *Parser) ExpectIdentifierMap() (map[string]string, error) {
	tok := p.GetToken()
	if tok == nil {
		return nil, p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACE {
		return nil, p.SyntaxError()
	}
	items := make(map[string]string, 0)
	for {
		tok := p.GetToken()
		var key string
		if tok == nil {
			return nil, p.EndOfFileError()
		}
		if tok.Type == CLOSE_BRACE {
			break
		}
		if tok.Type == SYMBOL {
			key = tok.Text
		} else if tok.Type == COMMA || tok.Type == NEWLINE || tok.Type == LINE_COMMENT {
			//ignore
			continue
		} else {
			return nil, p.SyntaxError()
		}
		err := p.expect(COLON)
		if err != nil {
			return nil, err
		}
		tok = p.GetToken()
		if tok == nil {
			return nil, p.EndOfFileError()
		}
		if tok.Type == CLOSE_BRACE {
			return nil, p.SyntaxError()
		}
		if tok.Type == SYMBOL {
			items[key] = tok.Text
		} else if tok.Type == COMMA || tok.Type == NEWLINE || tok.Type == LINE_COMMENT {
			//ignore
		} else {
			return nil, p.SyntaxError()
		}
	}
	return items, nil
}

func (p *Parser) MergeComment(comment1 string, comment2 string) string {
	if comment1 == "" {
		return TrimSpace(comment2)
	}
	return comment1 + "\n" + TrimSpace(comment2)
}

func (p *Parser) Error(msg string) error {
	Debug("*** error, last token:", p.lastToken)
	return fmt.Errorf("*** %s\n", FormattedAnnotation(p.path, p.source, "", msg, p.lastToken, RED, 5))
}

func (p *Parser) SyntaxError() error {
	return p.Error("Syntax error")
}

func (p *Parser) Warning(msg string) {
	fmt.Fprintf(os.Stderr, "[WARNING]: %s\n", FormattedAnnotation(p.path, p.source, "", msg, p.lastToken, RED, 5))
}

func (p *Parser) EndOfFileError() error {
	return p.Error("Unexpected end of file")
}

func (p *Parser) parseMetadata() error {
	key, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	err = p.expect(EQUALS)
	if err != nil {
		return err
	}
	val, err := p.parseLiteralValue()
	if err != nil {
		return err
	}
	if p.ast.Metadata == nil {
		p.ast.Metadata = data.NewObject()
	}
	p.ast.Metadata.Put(key, val)
	return nil
}

func (p *Parser) expectTarget() (string, error) {
	ident, err := p.expectNamespacedIdentifier()
	if err != nil {
		return "", err
	}
	tok := p.GetToken()
	if tok == nil {
		return ident, nil
	}
	if tok.Type != HASH {
		//check that the identifier is *not* a namespace, but jsut an identifier
		p.UngetToken()
		return ident, nil
	}
	ident = ident + "#"
	txt, err := p.expectText()
	if err != nil {
		return "", err
	}
	return ident + txt, nil
}

func (p *Parser) expectNamespacedIdentifier() (string, error) {
	txt, err := p.expectText()
	if err != nil {
		return "", err
	}
	ident := txt
	for {
		tok := p.GetToken()
		if tok == nil {
			break
		}
		if tok.Type != DOT {
			p.UngetToken()
			break
		}
		ident = ident + "."
		txt, err = p.expectText()
		if err != nil {
			return "", err
		}
		ident = ident + txt
	}
	return ident, nil
}

func (p *Parser) expectShapeId() (string, error) {
	txt, err := p.ExpectIdentifier()
	if err != nil {
		return "", err
	}
	ident := txt
	ns := ""
	mem := ""
	for {
		tok := p.GetToken()
		if tok == nil {
			break
		}
		if tok.Type != DOT {
			p.UngetToken()
			break
		}
		if ns == "" {
			ns = ident
		}
		ns = ns + "."
		ident = ""
		txt, err = p.ExpectIdentifier()
		if err != nil {
			return "", err
		}
		ns = ns + txt
	}
	for {
		tok := p.GetToken()
		if tok == nil {
			break
		}
		if tok.Type == HASH {
			if ns == "" {
				ns = ident
				ident = ""
			}
			if ns == "" || ident != "" || mem != "" {
				return "", p.SyntaxError()
			}
			key, err := p.ExpectIdentifier()
			if err != nil {
				return "", err
			}
			ident = key
		} else if tok.Type == DOLLAR {
			if ident == "" || mem != "" {
				return "", p.SyntaxError()
			}
			key, err := p.ExpectIdentifier()
			if err != nil {
				return "", err
			}
			mem = key
			break //nothing can come after this
		} else {
			p.UngetToken()
			break
		}
	}
	if mem != "" {
		ident = ident + "$" + mem
	}
	if ns != "" {
		ident = ns + "#" + ident
	}
	return ident, nil
}

func (p *Parser) parseNamespace(comment string) error {
	//	p.schema.Comment = p.MergeComment(p.schema.Comment, comment)
	if p.namespace != "" {
		return p.Error("Only one namespace per file allowed")
	}
	ns, err := p.expectNamespacedIdentifier()
	p.namespace = ns
	return err
}

func (p *Parser) addShapeDefinition(name string, shape *Shape) error {
	id := p.ensureNamespaced(name)
	if tmp := p.ast.GetShape(id); tmp != nil {
		return p.Error(fmt.Sprintf("Duplicate shape: %q", id))
	}
	if AnnotateSources {
		rpath := p.relativePath(p.path)
		shape.Traits, _ = withCommentTrait(shape.Traits, "", "source: "+rpath)
	}
	p.ast.PutShape(id, shape)
	return nil
}

func (p *Parser) parseSimpleTypeDef(typeName string, traits *data.Object) error {
	tname, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	enumItems := traits.GetArray("smithy.api#enum")
	if enumItems != nil {
		//convert to enum shape
		var tr *data.Object
		for _, k := range traits.Keys() {
			if k != "smithy.api#enum" {
				tr = withTrait(tr, k, traits.Get(k))
			}
		}
		enumShapeName := "enum"
		if typeName == "integer" {
			enumShapeName = "intEnum"
		}
		shape := &Shape{
			Type:   enumShapeName,
			Traits: tr,
		}
		mems := NewMembers()
		for _, e := range enumItems {
			var mtraits *data.Object
			d := data.AsObject(e)
			name := d.GetString("name") //optional
			if enumShapeName == "intEnum" {
				ivalue := d.GetInt("value") //required
				mtraits = withTrait(mtraits, "smithy.api#enumValue", ivalue)
			} else {
				svalue := d.GetString("value") //required
				if name == "" {
					name = svalue
					svalue = ""
				}
				if svalue != "" {
					mtraits = withTrait(mtraits, "smithy.api#enumValue", svalue)
				}
			}
			mems.Put(name, &Member{
				Target: "smithy.api#Unit",
				Traits: mtraits,
			})
		}
		shape.Members = mems
		return p.addShapeDefinition(tname, shape)
	}
	shape := &Shape{
		Type:   typeName,
		Traits: traits,
	}
	mixins, err := p.optionalMixins()
	if err != nil {
		return err
	}
	for _, mixin := range mixins {
		shape.Mixins = append(shape.Mixins, &ShapeRef{Target: p.ensureNamespaced(mixin)})
	}
	return p.addShapeDefinition(tname, shape)
}

func (p *Parser) optionalMixins() ([]string, error) {
	tok := p.GetToken()
	if tok == nil {
		return nil, nil
	}
	var mixins []string
	if tok.Type == SYMBOL && tok.Text == "with" {
		err := p.expect(OPEN_BRACKET)
		if err != nil {
			return nil, err
		}
		for {
			tok = p.GetToken()
			if tok == nil {
				return nil, p.EndOfFileError()
			}
			if tok.Type == CLOSE_BRACKET {
				break
			}
			if tok.Type == SYMBOL {
				mixins = append(mixins, tok.Text)
			}
		}
	} else {
		p.UngetToken()
	}
	return mixins, nil
}

func (p *Parser) parseList(traits *data.Object) error {
	sname := "list"
	name, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	tok := p.GetToken()
	if tok == nil {
		return p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACE {
		return p.SyntaxError()
	}
	shape := &Shape{
		Type:   sname,
		Traits: traits,
	}
	var mtraits *data.Object
	for {
		tok := p.GetToken()
		if tok == nil {
			return p.EndOfFileError()
		}
		if tok.Type == NEWLINE {
			continue
		}
		if tok.Type == CLOSE_BRACE {
			break
		}
		if tok.Type == AT {
			mtraits, err = p.parseTrait(mtraits)
			if err != nil {
				return err
			}
		} else if tok.Type == SYMBOL {
			fname := tok.Text
			err = p.expect(COLON)
			if err != nil {
				return err
			}
			if fname != "member" {
				return p.SyntaxError()
			}

			ftype, err := p.ExpectIdentifier()
			if err != nil {
				return err
			}
			err = p.ignore(COMMA)
			shape.Member = &Member{
				Target: p.ensureNamespaced(ftype),
				Traits: mtraits,
			}
			if shape.Member.Target == p.ensureNamespaced(name) {
				return p.Error(fmt.Sprintf("Directly recursive type references not allowed: %s", ftype))
			}
		} else {
			return p.SyntaxError()
		}
	}
	if shape.Member == nil {
		return p.Error("expected 'member' attribute, found none")
	}
	return p.addShapeDefinition(name, shape)
}

func (p *Parser) parseMap(sname string, traits *data.Object) error {
	name, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	tok := p.GetToken()
	if tok == nil {
		return p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACE {
		return p.SyntaxError()
	}
	shape := &Shape{
		Type:   sname,
		Traits: traits,
	}
	var mtraits *data.Object
	for {
		tok := p.GetToken()
		if tok == nil {
			return p.EndOfFileError()
		}
		if tok.Type == NEWLINE {
			continue
		}
		if tok.Type == CLOSE_BRACE {
			break
		}
		if tok.Type == AT {
			mtraits, err = p.parseTrait(mtraits)
			if err != nil {
				return err
			}
		} else if tok.Type == SYMBOL {
			fname := tok.Text
			err = p.expect(COLON)
			if err != nil {
				return err
			}
			ftype, err := p.ExpectIdentifier()
			if err != nil {
				return err
			}
			err = p.ignore(COMMA)
			if fname == "key" {
				shape.Key = &Member{
					Target: p.ensureNamespaced(ftype),
					Traits: mtraits,
				}
				if shape.Key.Target == p.ensureNamespaced(name) {
					return p.Error(fmt.Sprintf("Directly recursive type references not allowed: %s", ftype))
				}
				mtraits = nil
			} else if fname == "value" {
				shape.Value = &Member{
					Target: p.ensureNamespaced(ftype),
					Traits: mtraits,
				}
				if shape.Value.Target == p.ensureNamespaced(name) {
					return p.Error(fmt.Sprintf("Directly recursive type references not allowed: %s", ftype))
				}
				mtraits = nil
			} else {
				return p.SyntaxError()
			}
		} else {
			return p.SyntaxError()
		}
	}
	if shape.Key == nil {
		return p.Error("expected 'key' attribute, found none")
	}
	if shape.Value == nil {
		return p.Error("expected 'value' attribute, found none")
	}
	return p.addShapeDefinition(name, shape)
}

func (p *Parser) parseStructureBody(traits *data.Object) (*Shape, error) {
	shape := &Shape{
		Type:   "structure",
		Traits: traits,
	}
	mixins, err := p.optionalMixins()
	if err != nil {
		return nil, err
	}
	for _, mixin := range mixins {
		shape.Mixins = append(shape.Mixins, &ShapeRef{Target: p.ensureNamespaced(mixin)})
	}
	tok := p.GetToken()
	if tok == nil {
		return nil, p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACE {
		return nil, p.SyntaxError()
	}
	mems := NewMembers()
	comment := ""
	var mtraits *data.Object
	for {
		tok := p.GetToken()
		if tok == nil {
			return nil, p.EndOfFileError()
		}
		if tok.Type == NEWLINE {
			continue
		}
		if tok.Type == CLOSE_BRACE {
			break
		}
		if tok.Type == AT {
			mtraits, err = p.parseTrait(mtraits)
			if err != nil {
				return nil, err
			}
		} else if tok.Type == SYMBOL {
			fname := tok.Text
			err = p.expect(COLON)
			if err != nil {
				return nil, err
			}
			ftype, err := p.expectShapeId()
			if err != nil {
				return nil, err
			}
			err = p.ignore(COMMA)
			if comment != "" {
				mtraits, comment = withCommentTrait(mtraits, "", comment)
				comment = ""
			}
			mems.Put(fname, &Member{
				Target: p.ensureNamespaced(ftype),
				Traits: mtraits,
			})
			mtraits = nil
		} else if tok.Type == LINE_COMMENT {
			if strings.HasPrefix(tok.Text, "/") { //a triple slash means doc comment
				comment = p.MergeComment(comment, tok.Text[1:])
			}
		} else {
			return nil, p.SyntaxError()
		}
	}
	shape.Members = mems
	return shape, nil
}

func (p *Parser) parseStructure(traits *data.Object) error {
	name, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	body, err := p.parseStructureBody(traits)
	if err != nil {
		return err
	}
	return p.addShapeDefinition(name, body)
}

func (p *Parser) parseUnion(traits *data.Object) error {
	name, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	tok := p.GetToken()
	if tok == nil {
		return p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACE {
		return p.SyntaxError()
	}
	shape := &Shape{
		Type:   "union",
		Traits: traits,
	}
	mems := NewMembers()
	var mtraits *data.Object
	for {
		tok := p.GetToken()
		if tok == nil {
			return p.EndOfFileError()
		}
		if tok.Type == NEWLINE {
			continue
		}
		if tok.Type == CLOSE_BRACE {
			break
		}
		if tok.Type == AT {
			mtraits, err = p.parseTrait(mtraits)
			if err != nil {
				return err
			}
		} else if tok.Type == SYMBOL {
			fname := tok.Text
			err = p.expect(COLON)
			if err != nil {
				return err
			}
			ftype, err := p.expectShapeId()
			//ftype, err := p.expectTarget()
			if err != nil {
				return err
			}
			err = p.ignore(COMMA)
			mems.Put(fname, &Member{
				Target: p.ensureNamespaced(ftype),
				Traits: mtraits,
			})
			mtraits = nil
		} else {
			return p.SyntaxError()
		}
	}
	shape.Members = mems
	return p.addShapeDefinition(name, shape)
}

func (p *Parser) parseEnum(traits *data.Object, intEnum bool) error {
	name, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	tok := p.GetToken()
	if tok == nil {
		return p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACE {
		return p.SyntaxError()
	}
	tname := "enum"
	if intEnum {
		tname = "intEnum"
	}
	shape := &Shape{
		Type:   tname,
		Traits: traits,
	}
	mems := NewMembers()
	var mtraits *data.Object
	comment := ""
	for {
		tok := p.GetToken()
		if tok == nil {
			return p.EndOfFileError()
		}
		if tok.Type == NEWLINE {
			continue
		}
		if tok.Type == CLOSE_BRACE {
			break
		}
		if tok.Type == AT {
			mtraits, err = p.parseTrait(mtraits)
			if err != nil {
				return err
			}
		} else if tok.Type == SYMBOL {
			fname := tok.Text
			tok = p.GetToken()
			if tok == nil {
				return p.EndOfFileError()
			}
			if tok.Type == EQUALS {
				var v interface{}
				if intEnum {
					value, err := p.ExpectInt()
					if err != nil {
						return err
					}
					v = value
				} else {
					value, err := p.ExpectString()
					if err != nil {
						return err
					}
					v = value
				}
				mtraits = withTrait(mtraits, "smithy.api#enumValue", v)
			} else {
				p.UngetToken()
			}
			err = p.ignore(COMMA)
			mtraits, comment = withCommentTrait(mtraits, "", comment)
			mems.Put(fname, &Member{
				Target: "smithy.api#Unit",
				Traits: mtraits,
			})
			mtraits = nil
			comment = ""
		} else if tok.Type == LINE_COMMENT {
			if strings.HasPrefix(tok.Text, "/") { //a triple slash means doc comment
				comment = p.MergeComment(comment, tok.Text[1:])
			}
		} else {
			return p.SyntaxError()
		}
	}
	shape.Members = mems
	return p.addShapeDefinition(name, shape)
}

func (p *Parser) parseOperation(traits *data.Object) error {
	name, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	tok := p.GetToken()
	if tok == nil {
		return p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACE {
		return p.SyntaxError()
	}
	shape := &Shape{
		Type:   "operation",
		Traits: traits,
	}
	for {
		tok := p.GetToken()
		if tok == nil {
			return p.EndOfFileError()
		}
		if tok.Type == NEWLINE {
			continue
		}
		if tok.Type == CLOSE_BRACE {
			break
		}
		if tok.Type == LINE_COMMENT {
			continue
		}
		if tok.Type != COLON {
			p.UngetToken()
		}
		fname, err := p.ExpectIdentifier()
		if err != nil {
			return err
		}
		err = p.expect(COLON)
		if err != nil {
			return err
		}
		switch fname {
		case "input":
			tok := p.GetToken()
			if tok == nil {
				return p.EndOfFileError()
			}
			if tok.Type == EQUALS {
				if p.version < 2 {
					err = p.SyntaxError()
				} else {
					traits = data.ObjectFromMap(map[string]interface{}{"smithy.api#input": data.NewObject()})
					body, err := p.parseStructureBody(traits)
					if err != nil {
						return err
					}
					inName := name + "Input"
					shape.Input = &ShapeRef{Target: p.ensureNamespaced(inName)}
					p.addShapeDefinition(inName, body)
				}
			} else {
				p.UngetToken()
				shape.Input, err = p.expectShapeRef()
			}
		case "output":
			tok := p.GetToken()
			if tok == nil {
				return p.EndOfFileError()
			}
			if tok.Type == EQUALS {
				if p.version < 2 {
					err = p.SyntaxError()
				} else {
					traits = data.ObjectFromMap(map[string]interface{}{"smithy.api#output": data.NewObject()})
					body, err := p.parseStructureBody(traits)
					if err != nil {
						return err
					}
					outName := name + "Output"
					shape.Output = &ShapeRef{Target: p.ensureNamespaced(outName)}
					p.addShapeDefinition(outName, body)
				}
			} else {
				p.UngetToken()
				shape.Output, err = p.expectShapeRef()
			}
		case "errors":
			shape.Errors, err = p.expectShapeRefs()
		default:
			return p.SyntaxError()
		}
		if err != nil {
			return err
		}
		err = p.ignore(COMMA)
	}
	return p.addShapeDefinition(name, shape)
}

func (p *Parser) parseService(traits *data.Object) error {
	name, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	tok := p.GetToken()
	if tok == nil {
		return p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACE {
		return p.SyntaxError()
	}
	shape := &Shape{
		Type:   "service",
		Traits: traits,
	}
	for {
		tok := p.GetToken()
		if tok == nil {
			return p.EndOfFileError()
		}
		if tok.Type == NEWLINE {
			continue
		}
		if tok.Type == CLOSE_BRACE {
			break
		}
		if tok.Type != COLON {
			p.UngetToken()
		}
		fname, err := p.ExpectIdentifier()
		if err != nil {
			return err
		}
		err = p.expect(COLON)
		if err != nil {
			return err
		}
		switch fname {
		case "version":
			shape.Version, err = p.ExpectString()
		case "operations":
			shape.Operations, err = p.expectShapeRefs()
		case "resources":
			shape.Resources, err = p.expectShapeRefs()
		default:
			return p.SyntaxError()
		}
		if err != nil {
			return err
		}
		err = p.ignore(COMMA)
	}
	return p.addShapeDefinition(name, shape)
}

func (p *Parser) parseResource(traits *data.Object) error {
	name, err := p.ExpectIdentifier()
	if err != nil {
		return err
	}
	tok := p.GetToken()
	if tok == nil {
		return p.EndOfFileError()
	}
	if tok.Type != OPEN_BRACE {
		return p.SyntaxError()
	}
	shape := &Shape{
		Type:   "resource",
		Traits: traits,
	}
	var comment string
	traits, comment = withCommentTrait(traits, "", comment)
	for {
		tok := p.GetToken()
		if tok == nil {
			return p.EndOfFileError()
		}
		if tok.Type == NEWLINE {
			continue
		}
		if tok.Type == CLOSE_BRACE {
			break
		}
		if tok.Type == LINE_COMMENT {
			if strings.HasPrefix(tok.Text, "/") { //a triple slash means doc comment
				comment = p.MergeComment(comment, tok.Text[1:])
			}
			continue
		} else {
			p.UngetToken()
		}
		fname, err := p.ExpectIdentifier()
		if err != nil {
			return err
		}
		err = p.expect(COLON)
		if err != nil {
			return err
		}
		switch fname {
		case "identifiers":
			shape.Identifiers, err = p.expectNamedShapeRefs()
		case "create":
			shape.Create, err = p.expectShapeRef()
		case "put":
			shape.Put, err = p.expectShapeRef()
		case "read":
			shape.Read, err = p.expectShapeRef()
		case "update":
			shape.Update, err = p.expectShapeRef()
		case "delete":
			shape.Delete, err = p.expectShapeRef()
		case "list":
			shape.Delete, err = p.expectShapeRef()
		case "operations":
			shape.Operations, err = p.expectShapeRefs()
		case "collectionOperations":
			shape.CollectionOperations, err = p.expectShapeRefs()
		case "Resources":
			shape.Resources, err = p.expectShapeRefs()
		default:
			return p.SyntaxError()
		}
		if err != nil {
			return err
		}
		err = p.ignore(COMMA)
	}
	return p.addShapeDefinition(name, shape)
}

func IsPreludeType(name string) bool {
	switch name {
	case "Boolean", "PrimitiveBoolean", "String", "Blob", "Timestamp", "Document", "BigInteger", "BigDecimal":
		return true
	case "Byte", "Short", "Integer", "Long", "Float", "Double":
		return true
		/* v1 only, v2 does not support Primitive types, nor the boxed trait.
		   case "PrimitiveByte", "PrimitiveShort", "PrimitiveInteger", "PrimitiveLong", "PrimitiveFloat", "PrimitiveDouble":
		return true
		*/
	}
	return false
}

func (p *Parser) ensureNamespaced(name string) string {
	if IsPreludeType(name) {
		return "smithy.api#" + name
	}
	if strings.Index(name, "#") < 0 {
		if full, ok := p.use[name]; ok {
			return full
		}
		return p.namespace + "#" + name
	}
	return name
}

func (p *Parser) expectNamedShapeRefs() (map[string]*ShapeRef, error) {
	targets, err := p.ExpectIdentifierMap()
	if err != nil {
		return nil, err
	}
	refs := make(map[string]*ShapeRef, 0)
	for k, target := range targets {
		ref := &ShapeRef{
			Target: p.ensureNamespaced(target),
		}
		refs[k] = ref
	}
	return refs, nil
}

func (p *Parser) expectShapeRefs() ([]*ShapeRef, error) {
	targets, err := p.ExpectIdentifierArray()
	if err != nil {
		return nil, err
	}
	var refs []*ShapeRef
	for _, target := range targets {
		ref := &ShapeRef{
			Target: p.ensureNamespaced(target),
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func (p *Parser) expectShapeRef() (*ShapeRef, error) {
	tname, err := p.ExpectIdentifier()
	if err != nil {
		return nil, err
	}
	ref := &ShapeRef{
		Target: p.ensureNamespaced(tname),
	}
	return ref, nil
}

func (p *Parser) parseTraitArgs() (*data.Object, interface{}, error) {
	var err error
	args := data.NewObject()
	var literal interface{}
	tok := p.GetToken()
	if tok == nil {
		return args, nil, nil
	}
	if tok.Type == OPEN_PAREN {
		for {
			tok := p.GetToken()
			if tok == nil {
				return nil, nil, p.SyntaxError()
			}
			if tok.Type == CLOSE_PAREN {
				return args, literal, nil
			}
			if tok.Type == LINE_COMMENT {
				continue
			}
			if tok.Type == SYMBOL {
				p.ignore(COLON)
				val, err := p.parseLiteralValue()
				if err != nil {
					return nil, nil, err
				}
				args = withTrait(args, tok.Text, val)
			} else if tok.Type == OPEN_BRACKET {
				literal, err = p.parseLiteralArray()
				if err != nil {
					return nil, nil, err
				}
			} else if tok.Type == COMMA || tok.Type == NEWLINE {
				//ignore
			} else {
				return nil, nil, p.SyntaxError()
			}
		}
	} else {
		p.UngetToken()
		return args, nil, nil
	}
}

func (p *Parser) parseTrait(traits *data.Object) (*data.Object, error) {
	traitName, err := p.expectShapeId()
	if err != nil {
		return traits, err
	}
	tv, ok := p.visitors[traitName]
	if !ok {
		if tv, ok = p.visitors[GenericAccept]; !ok {
			return traits, p.SyntaxError()
		}
	}
	return tv.Parse(wrappedParser{p: p}, traitName, traits)
}

func withTrait(traits *data.Object, key string, val interface{}) *data.Object {
	if val != nil {
		if traits == nil {
			traits = data.NewObject()
		}
		traits.Put(key, val)
	}
	return traits
}

func withCommentTrait(traits *data.Object, namespace string, val string) (*data.Object, string) {
	if namespace == "" {
		namespace = "smithy.api#documentation"
	}

	if val != "" {
		val = TrimSpace(val)
		traits = withTrait(traits, namespace, val)
	}
	return traits, ""
}

func (p *Parser) parseLiteralValue() (interface{}, error) {
	tok := p.GetToken()
	if tok == nil {
		return nil, p.SyntaxError()
	}
	return p.parseLiteral(tok)
}

func (p *Parser) parseLiteral(tok *Token) (interface{}, error) {
	switch tok.Type {
	case SYMBOL:
		return p.parseLiteralSymbol(tok)
	case STRING:
		//todo: string blocks, i.e. triple-quoted strings
		return p.parseLiteralString(tok)
	case NUMBER:
		return p.parseLiteralNumber(tok)
	case OPEN_BRACKET:
		return p.parseLiteralArray()
	case OPEN_BRACE:
		return p.parseLiteralObject()
	default:
		return nil, p.SyntaxError()
	}
}

func (p *Parser) parseLiteralSymbol(tok *Token) (interface{}, error) {
	switch tok.Text {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	default:
		return nil, p.Error(fmt.Sprintf("Not a valid symbol: %s", tok.Text))
	}
}
func (p *Parser) parseLiteralString(tok *Token) (*string, error) {
	return &tok.Text, nil
}

func (p *Parser) parseLiteralNumber(tok *Token) (interface{}, error) {
	num, err := data.ParseDecimal(tok.Text)
	if err != nil {
		return nil, p.Error(fmt.Sprintf("Not a valid number: %s", tok.Text))
	}
	return num, nil
}

func (p *Parser) parseLiteralArray() (interface{}, error) {
	var ary []interface{}
	for {
		tok := p.GetToken()
		if tok == nil {
			return nil, p.EndOfFileError()
		}
		if tok.Type != NEWLINE {
			if tok.Type == CLOSE_BRACKET {
				return ary, nil
			}
			if tok.Type == LINE_COMMENT {
				continue
			}
			if tok.Type != COMMA {
				obj, err := p.parseLiteral(tok)
				if err != nil {
					return nil, err
				}
				ary = append(ary, obj)
			}
		}
	}
}

func (p *Parser) parseLiteralObject() (interface{}, error) {
	//either a map or a struct, i.e. a JSON object
	obj := make(map[string]interface{}, 0)
	for {
		tok := p.GetToken()
		if tok == nil {
			return nil, p.EndOfFileError()
		}
		if tok.Type == CLOSE_BRACE {
			return obj, nil
		}
		if tok.IsText() {
			key := tok.Text
			err := p.expect(COLON)
			if err != nil {
				return nil, err
			}
			val, err := p.parseLiteralValue()
			if err != nil {
				return nil, err
			}
			obj[key] = val
		} else if tok.Type == SYMBOL {
			return nil, p.Error("Expected String or Identifier key for NodeObject, found symbol '" + tok.Text + "'")
		} else {
			//fmt.Println("ignoring this token:", tok)
		}
	}
}

func StripNamespace(target string) string {
	n := strings.Index(target, "#")
	if n < 0 {
		return target
	}
	return target[n+1:]
}

func (p *Parser) relativePath(path string) string {
	if !strings.HasPrefix(path, "/") {
		return path
	}
	if !strings.HasPrefix(path, p.wd) {
		p1 := strings.Split(path, "/")
		p2 := strings.Split(p.wd, "/")
		i := 0
		for p1[i] == p2[i] {
			p1 = p1[1:]
			p2 = p2[1:]
		}
		s := strings.Join(p1, "/")
		for _, _ = range p2 {
			s = "../" + s
		}
		return s
	} else {
		i := len(p.wd)
		return path[i:]
	}
}

type wrappedParser struct {
	p *Parser
}

func (w wrappedParser) EnsureNamespaced(name string) string { return w.p.ensureNamespaced(name) }

func (w wrappedParser) Expect(tokenType TokenType) error { return w.p.expect(tokenType) }

func (w wrappedParser) ExpectIdentifier() (string, error) { return w.p.ExpectIdentifier() }

func (w wrappedParser) ExpectNumber() (*data.Decimal, error) { return w.p.ExpectNumber() }

func (w wrappedParser) ExpectInt() (int, error) { return w.p.ExpectInt() }

func (w wrappedParser) ExpectString() (string, error) { return w.p.ExpectString() }

func (w wrappedParser) ExpectStringArray() ([]string, error) { return w.p.ExpectStringArray() }

func (w wrappedParser) ExpectIdentifierArray() ([]string, error) { return w.p.ExpectIdentifierArray() }

func (w wrappedParser) ExpectIdentifierMap() (map[string]string, error) {
	return w.p.ExpectIdentifierMap()
}

func (w wrappedParser) GetToken() *Token { return w.p.GetToken() }

func (w wrappedParser) MergeComment(comment1 string, comment2 string) string {
	return w.p.MergeComment(comment1, comment2)
}

func (w wrappedParser) ParseTraitArgs() (*data.Object, interface{}, error) {
	return w.p.parseTraitArgs()
}

func (w wrappedParser) WithTrait(traits *data.Object, key string, val interface{}) *data.Object {
	return withTrait(traits, key, val)
}

func (w wrappedParser) WithCommentTrait(traits *data.Object, namespace string, val string) (*data.Object, string) {
	return withCommentTrait(traits, namespace, val)
}

func (w wrappedParser) Error(msg string) error { return w.p.Error(msg) }

func (w wrappedParser) SyntaxError() error { return w.p.SyntaxError() }

func (w wrappedParser) Warning(msg string) { w.p.Warning(msg) }

func (w wrappedParser) EndOfFileError() error { return w.p.EndOfFileError() }
