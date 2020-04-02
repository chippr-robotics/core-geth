// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package rpc

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"math/big"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/alecthomas/jsonschema"
	"github.com/davecgh/go-spew/spew"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/jst"
	"github.com/go-openapi/spec"
	goopenrpcT "github.com/gregdhill/go-openrpc/types"
)

func (s *RPCService) Describe() (*goopenrpcT.OpenRPCSpec1, error) {

	if s.doc == nil {
		s.doc = NewOpenRPCDescription(s.server)
	}

	for module, list := range s.methods() {
		if module == "rpc" {
			continue
		}

	methodListLoop:
		for _, methodName := range list {
			fullName := strings.Join([]string{module, methodName}, serviceMethodSeparators[0])
			method := s.server.services.services[module].callbacks[methodName]

			// FIXME: Development only.
			// There is a bug with the isPubSub method, it's not picking up #PublicEthAPI.eth_subscribeSyncStatus
			// because the isPubSub conditionals are wrong or the method is wrong.
			if method.isSubscribe || strings.Contains(fullName, subscribeMethodSuffix) {
				continue
			}

			// Dedupe. Not sure how `admin_datadir` got in there twice.
			for _, m := range s.doc.Doc.Methods {
				if m.Name == fullName {
					continue methodListLoop
				}
			}
			if err := s.doc.RegisterMethod(fullName, method); err != nil {
				return nil, err
			}
		}
	}

	if err := Clean(s.doc.Doc); err != nil {
		panic(err.Error())
	}

	return s.doc.Doc, nil
}

// ---

type OpenRPCDescription struct {
	Doc *goopenrpcT.OpenRPCSpec1
}

func NewOpenRPCDescription(server *Server) *OpenRPCDescription {
	doc := &goopenrpcT.OpenRPCSpec1{
		OpenRPC: "1.2.4",
		Info: goopenrpcT.Info{
			Title:          "Ethereum JSON-RPC",
			Description:    "This API lets you interact with an EVM-based client via JSON-RPC",
			TermsOfService: "https://github.com/etclabscore/core-geth/blob/master/COPYING",
			Contact: goopenrpcT.Contact{
				Name:  "",
				URL:   "",
				Email: "",
			},
			License: goopenrpcT.License{
				Name: "Apache-2.0",
				URL:  "https://www.apache.org/licenses/LICENSE-2.0.html",
			},
			Version: "1.0.10",
		},
		Servers: []goopenrpcT.Server{},
		Methods: []goopenrpcT.Method{},
		Components: goopenrpcT.Components{
			ContentDescriptors:    make(map[string]*goopenrpcT.ContentDescriptor),
			Schemas:               make(map[string]spec.Schema),
			Examples:              make(map[string]goopenrpcT.Example),
			Links:                 make(map[string]goopenrpcT.Link),
			Errors:                make(map[string]goopenrpcT.Error),
			ExamplePairingObjects: make(map[string]goopenrpcT.ExamplePairing),
			Tags:                  make(map[string]goopenrpcT.Tag),
		},
		ExternalDocs: goopenrpcT.ExternalDocs{
			Description: "Source",
			URL:         "https://github.com/etclabscore/core-geth",
		},
		Objects: goopenrpcT.NewObjectMap(),
	}

	return &OpenRPCDescription{Doc: doc}
}

// Clean makes the openrpc validator happy.
// FIXME: Name me something better/organize me better.
func Clean(doc *goopenrpcT.OpenRPCSpec1) error {
	a := jst.NewAnalysisT()
	a.TraverseOptions = jst.TraverseOptions{ExpandAtNode: true}

	uniqueKeyFn := func(sch spec.Schema) string {
		b, _ := json.Marshal(sch)
		sum := sha1.Sum(b)
		out := fmt.Sprintf("%x", sum[:4])

		if sch.Title != "" {
			out = fmt.Sprintf("%s.", sch.Title) + out
		}

		if len(sch.Type) != 0 {
			out = fmt.Sprintf("%s.", strings.Join(sch.Type, "+")) + out
		}

		spl := strings.Split(sch.Description, ":")
		splv := spl[len(spl)-1]
		if splv != "" && !strings.Contains(splv, " ") {
			out = splv + "_" + out
		}

		return out
	}

	registerSchema := func(leaf spec.Schema) error {
		a.RegisterSchema(leaf, uniqueKeyFn)
		return nil
	}

	mustMarshalString := func(v interface{}) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	doc.Components.Schemas = make(map[string]spec.Schema)
	for im := 0; im < len(doc.Methods); im++ {

		met := doc.Methods[im]
		fmt.Println(met.Name)

		referencer := func(sch *spec.Schema) error {

			err := registerSchema(*sch)
			if err != nil {
				fmt.Println("!!! ", err)
				return err
			}

			fmt.Println("   *", mustMarshalString(sch))

			r, err := a.SchemaAsReferenceSchema(*sch)
			if err != nil {
				fmt.Println("error getting schema as ref-only schema")
				return err
			}

			doc.Components.Schemas[uniqueKeyFn(*sch)] = *sch
			*sch = r

			fmt.Println("    @", mustMarshalString(sch))
			return nil
		}

		/*
		removeDefinitions is a workaround to get rid of definitions at each schema,
		instead of doing what we probably should which is updating the reference uri against
		the document root
		*/
		removeDefinitions := func(sch *spec.Schema) error {
			sch.Definitions = nil
			return nil
		}

		// Params.
		for ip := 0; ip < len(met.Params); ip++ {
			par := met.Params[ip]
			fmt.Println(" < ", par.Name)

			a.Traverse(&par.Schema, removeDefinitions)
			a.Traverse(&par.Schema, referencer)
			met.Params[ip] = par
			fmt.Println("   :", mustMarshalString(par))
		}

		// Result (single).
		//fmt.Println(" > ", doc.Methods[im].Result.Name)

		a.Traverse(&met.Result.Schema, removeDefinitions)
		a.Traverse(&met.Result.Schema, referencer)
		fmt.Println("   :", mustMarshalString(&met.Result))
	}

	return nil
}

func (d *OpenRPCDescription) RegisterMethod(name string, cb *callback) error {

	cb.makeArgTypes()
	cb.makeRetTypes()

	rtFunc := runtime.FuncForPC(cb.fn.Pointer())
	cbFile, _ := rtFunc.FileLine(rtFunc.Entry())

	tokenset := token.NewFileSet()
	astFile, err := parser.ParseFile(tokenset, cbFile, nil, parser.ParseComments)
	if err != nil {
		return err
	}

	astFuncDel := getAstFunc(cb, astFile, rtFunc)

	if astFuncDel == nil {
		return fmt.Errorf("nil ast func: method name: %s", name)
	}

	method, err := makeMethod(name, cb, rtFunc, astFuncDel)
	if err != nil {
		return fmt.Errorf("make method error method=%s cb=%s error=%v", name, spew.Sdump(cb), err)
	}

	d.Doc.Methods = append(d.Doc.Methods, method)
	sort.Slice(d.Doc.Methods, func(i, j int) bool {
		return d.Doc.Methods[i].Name < d.Doc.Methods[j].Name
	})

	return nil
}

type argIdent struct {
	ident *ast.Ident
	name  string
}

func (a argIdent) Name() string {
	if a.ident != nil {
		return a.ident.Name
	}
	return a.name
}

//// analysisOnLeaf runs a callback function on each leaf of a the JSON schema tree.
//// It will return the first error it encounters.
//func (a *jst.AnalysisT) analysisOnLeaf(sch spec.Schema, onLeaf func(leaf spec.Schema) error) error {
//	for i := range sch.Definitions {
//		a.analysisOnLeaf(sch.Definitions[i], onLeaf)
//	}
//	for i := range sch.OneOf {
//		a.analysisOnLeaf(sch.OneOf[i], onLeaf)
//	}
//	for i := range sch.AnyOf {
//		a.analysisOnLeaf(sch.AnyOf[i], onLeaf)
//	}
//	for i := range sch.AllOf {
//		a.analysisOnLeaf(sch.AllOf[i], onLeaf)
//	}
//	for k := range sch.Properties {
//		a.analysisOnLeaf(sch.Properties[k], onLeaf)
//	}
//	for k := range sch.PatternProperties {
//		a.analysisOnLeaf(sch.PatternProperties[k], onLeaf)
//	}
//	if sch.Items == nil {
//		return onLeaf(sch)
//	}
//	if sch.Items.Len() > 1 {
//		for i := range sch.Items.Schemas {
//			a.analysisOnLeaf(sch.Items.Schemas[i], onLeaf) // PTAL: Is this right?
//		}
//	} else {
//		// Is schema
//		a.analysisOnLeaf(*sch.Items.Schema, onLeaf)
//	}
//	return onLeaf(sch)
//}

func makeMethod(name string, cb *callback, rt *runtime.Func, fn *ast.FuncDecl) (goopenrpcT.Method, error) {
	file, line := rt.FileLine(rt.Entry())

	//packageName := strings.Split(rt.Name(), ".")[0]

	m := goopenrpcT.Method{
		Name:        name,
		Tags:        []goopenrpcT.Tag{},
		Summary:     fn.Doc.Text(),
		Description: "", // fmt.Sprintf(`%s@%s:%d'`, rt.Name(), file, line),
		ExternalDocs: goopenrpcT.ExternalDocs{
			Description: rt.Name(),
			URL:         fmt.Sprintf("file://%s:%d", file, line),
		},
		Params:         []*goopenrpcT.ContentDescriptor{},
		Result:         &goopenrpcT.ContentDescriptor{},
		Deprecated:     false,
		Servers:        []goopenrpcT.Server{},
		Errors:         []goopenrpcT.Error{},
		Links:          []goopenrpcT.Link{},
		ParamStructure: "by-position",
		Examples:       []goopenrpcT.ExamplePairing{},
	}

	defer func() {
		if m.Result.Name == "" {
			m.Result.Name = "null"
			m.Result.Schema.Type = []string{"null"}
			m.Result.Schema.Description = "Null"
		}
	}()

	if fn.Type.Params != nil {
		j := 0
		for _, field := range fn.Type.Params.List {
			if field == nil {
				continue
			}
			if cb.hasCtx && strings.Contains(fmt.Sprintf("%s", field.Type), "context") {
				continue
			}
			if len(field.Names) > 0 {
				for _, ident := range field.Names {
					if ident == nil {
						continue
					}
					if j > len(cb.argTypes)-1 {
						log.Println(name, cb.argTypes, field.Names, j)
						continue
					}
					cd, err := makeContentDescriptor(cb.argTypes[j], field, argIdent{ident, fmt.Sprintf("%sParameter%d", name, j)})
					if err != nil {
						return m, err
					}
					j++
					m.Params = append(m.Params, &cd)
				}
			} else {
				cd, err := makeContentDescriptor(cb.argTypes[j], field, argIdent{nil, fmt.Sprintf("%sParameter%d", name, j)})
				if err != nil {
					return m, err
				}
				j++
				m.Params = append(m.Params, &cd)
			}

		}
	}
	if fn.Type.Results != nil {
		j := 0
		for _, field := range fn.Type.Results.List {
			if field == nil {
				continue
			}
			if strings.Contains(fmt.Sprintf("%s", field.Type), "error") {
				continue
			}
			if len(field.Names) > 0 {
				// This really should never ever happen I don't think.
				// JSON-RPC returns _an_ result. So there can't be > 1 return value.
				// But just in case.
				for _, ident := range field.Names {
					cd, err := makeContentDescriptor(cb.retTypes[j], field, argIdent{ident, fmt.Sprintf("%sResult%d", name, j)})
					if err != nil {
						return m, err
					}
					j++
					m.Result = &cd
				}
			} else {
				cd, err := makeContentDescriptor(cb.retTypes[j], field, argIdent{nil, fmt.Sprintf("%sResult", name)})
				if err != nil {
					return m, err
				}
				j++
				m.Result = &cd
			}
		}
	}

	return m, nil
}

func makeContentDescriptor(ty reflect.Type, field *ast.Field, ident argIdent) (goopenrpcT.ContentDescriptor, error) {
	cd := goopenrpcT.ContentDescriptor{}
	if !jsonschemaPkgSupport(ty) {
		return cd, fmt.Errorf("unsupported iface: %v %v %v", spew.Sdump(ty), spew.Sdump(field), spew.Sdump(ident))
	}

	schemaType := fmt.Sprintf("%s:%s", ty.PkgPath(), ty.Name())
	switch tt := field.Type.(type) {
	case *ast.SelectorExpr:
		schemaType = fmt.Sprintf("%v.%v", tt.X, tt.Sel)
		schemaType = fmt.Sprintf("%s:%s", ty.PkgPath(), schemaType)
	case *ast.StarExpr:
		schemaType = fmt.Sprintf("%v", tt.X)
		schemaType = fmt.Sprintf("*%s:%s", ty.PkgPath(), schemaType)
		if reflect.ValueOf(ty).Type().Kind() == reflect.Ptr {
			schemaType = fmt.Sprintf("%v", ty.Elem().Name())
			schemaType = fmt.Sprintf("*%s:%s", ty.Elem().PkgPath(), schemaType)
		}
		//ty = ty.Elem() // FIXME: wart warn
	}
	//schemaType = fmt.Sprintf("%s:%s", ty.PkgPath(), schemaType)

	//cd.Name = schemaType
	cd.Name = ident.Name()

	cd.Summary = field.Doc.Text()
	cd.Description = field.Comment.Text()

	rflctr := jsonschema.Reflector{
		AllowAdditionalProperties:  false, // false,
		RequiredFromJSONSchemaTags: true,
		ExpandedStruct:             false, // false, // false,
		//IgnoredTypes:               []interface{}{chaninterface},
		TypeMapper: OpenRPCJSONSchemaTypeMapper,
	}

	jsch := rflctr.ReflectFromType(ty)

	// Poor man's type cast.
	// Need to get the type from the go struct -> json reflector package
	// to the swagger/go-openapi/jsonschema spec.
	// Do this with JSON marshaling.
	// Hacky? Maybe. Effective? Maybe.
	m, err := json.Marshal(jsch)
	if err != nil {
		log.Fatal(err)
	}
	sch := spec.Schema{}
	err = json.Unmarshal(m, &sch)
	if err != nil {
		log.Fatal(err)
	}
	// End Hacky maybe.
	if schemaType != ":" && (cd.Schema.Description == "" || cd.Schema.Description == ":") {
		sch.Description = schemaType
	}

	cd.Schema = sch

	return cd, nil
}

func jsonschemaPkgSupport(r reflect.Type) bool {
	switch r.Kind() {
	case reflect.Struct,
		reflect.Map,
		reflect.Slice, reflect.Array,
		reflect.Interface,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64,
		reflect.Bool,
		reflect.String,
		reflect.Ptr:
		return true
	default:
		return false
	}
}

type schemaDictEntry struct {
	t interface{}
	j string
}

func OpenRPCJSONSchemaTypeMapper(r reflect.Type) *jsonschema.Type {
	unmarshalJSONToJSONSchemaType := func(input string) *jsonschema.Type {
		var js jsonschema.Type
		err := json.Unmarshal([]byte(input), &js)
		if err != nil {
			return nil
		}
		return &js
	}

	integerD := `{
          "title": "integer",
          "type": "string",
          "pattern": "^0x[a-fA-F0-9]+$",
          "description": "Hex representation of the integer"
        }`
	commonHashD := `{
          "title": "keccak",
          "type": "string",
          "description": "Hex representation of a Keccak 256 hash",
          "pattern": "^0x[a-fA-F\\d]{64}$"
        }`
	blockNumberTagD := `{
          "title": "blockNumberTag",
          "type": "string",
          "description": "The optional block height description",
          "enum": [
            "earliest",
            "latest",
            "pending"
          ]
        }`

	//s := jsonschema.Reflect(ethapi.Progress{})
	//ethSyncingResultProgress, err := json.Marshal(s)
	//if err != nil {
	//	return nil
	//}

	// Second, handle other types.
	// Use a slice instead of a map because it preserves order, as a logic safeguard/fallback.
	dict := []schemaDictEntry{

		{new(big.Int), integerD},
		{big.Int{}, integerD},
		{new(hexutil.Big), integerD},
		{hexutil.Big{}, integerD},

		{types.BlockNonce{}, integerD},

		{new(common.Address), `{
          "title": "keccak",
          "type": "string",
          "description": "Hex representation of a Keccak 256 hash POINTER",
          "pattern": "^0x[a-fA-F\\d]{64}$"
        }`},

		{common.Address{}, `{
          "title": "address",
          "type": "string",
          "pattern": "^0x[a-fA-F\\d]{40}$"
        }`},

		{new(common.Hash), `{
          "title": "keccak",
          "type": "string",
          "description": "Hex representation of a Keccak 256 hash POINTER",
          "pattern": "^0x[a-fA-F\\d]{64}$"
        }`},

		{common.Hash{}, commonHashD},

		{
			hexutil.Bytes{}, `{
          "title": "dataWord",
          "type": "string",
          "description": "Hex representation of a 256 bit unit of data",
          "pattern": "^0x([a-fA-F\\d]{64})?$"
        }`},
		{
			new(hexutil.Bytes), `{
          "title": "dataWord",
          "type": "string",
          "description": "Hex representation of a 256 bit unit of data",
          "pattern": "^0x([a-fA-F\\d]{64})?$"
        }`},

		{[]byte{}, `{
          "title": "bytes",
          "type": "string",
          "description": "Hex representation of a variable length byte array",
          "pattern": "^0x([a-fA-F0-9]?)+$"
        }`},

		{BlockNumberOrHash{}, fmt.Sprintf(`{
		  "title": "blockNumberOrHash",
		  "description": "Hex representation of a block number or hash",
		  "oneOf": [%s, %s]
		}`, commonHashD, integerD)},

		{BlockNumber(0), fmt.Sprintf(`{
		  "title": "blockNumberOrTag",
		  "description": "Block tag or hex representation of a block number",
		  "oneOf": [%s, %s]
		}`, commonHashD, blockNumberTagD)},

		//		{ethapi.EthSyncingResult{}, fmt.Sprintf(`{
		//          "title": "ethSyncingResult",
		//		  "description": "Syncing returns false in case the node is currently not syncing with the network. It can be up to date or has not
		//yet received the latest block headers from its pears. In case it is synchronizing:
		//- startingBlock: block number this node started to synchronise from
		//- currentBlock:  block number this node is currently importing
		//- highestBlock:  block number of the highest block header this node has received from peers
		//- pulledStates:  number of state entries processed until now
		//- knownStates:   number of known state entries that still need to be pulled",
		//		  "oneOf": [%s, %s]
		//		}`, `{
		//        "type": "boolean"
		//      }`, `{"type": "object"}`)},

	}

	for _, d := range dict {
		d := d
		if reflect.TypeOf(d.t) == r {
			tt := unmarshalJSONToJSONSchemaType(d.j)

			return tt
		}
	}

	// First, handle primitives.
	switch r.Kind() {
	case reflect.Struct:

	case reflect.Map,
		reflect.Interface:
	case reflect.Slice, reflect.Array:

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		ret := unmarshalJSONToJSONSchemaType(integerD)
		return ret

	case reflect.Float32, reflect.Float64:

	case reflect.Bool:

	case reflect.String:

	case reflect.Ptr:

	default:
		panic("prevent me somewhere else please")
	}

	return nil
}

func getAstFunc(cb *callback, astFile *ast.File, rf *runtime.Func) *ast.FuncDecl {

	rfName := runtimeFuncName(rf)
	for _, decl := range astFile.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name == nil || fn.Name.Name != rfName {
			continue
		}
		//log.Println("getAstFunc", spew.Sdump(cb), spew.Sdump(fn))
		fnRecName := ""
		for _, l := range fn.Recv.List {
			if fnRecName != "" {
				break
			}
			i, ok := l.Type.(*ast.Ident)
			if ok {
				fnRecName = i.Name
				continue
			}
			s, ok := l.Type.(*ast.StarExpr)
			if ok {
				fnRecName = fmt.Sprintf("%v", s.X)
			}
		}
		// Ensure that this is the one true function.
		// Have to match receiver AND method names.
		/*
		 => recvr= <*ethapi.PublicBlockChainAPI Value> fn= PublicBlockChainAPI
		 => recvr= <*ethash.API Value> fn= API
		 => recvr= <*ethapi.PublicTxPoolAPI Value> fn= PublicTxPoolAPI
		 => recvr= <*ethapi.PublicTxPoolAPI Value> fn= PublicTxPoolAPI
		 => recvr= <*ethapi.PublicTxPoolAPI Value> fn= PublicTxPoolAPI
		 => recvr= <*ethapi.PublicNetAPI Value> fn= PublicNetAPI
		 => recvr= <*ethapi.PublicNetAPI Value> fn= PublicNetAPI
		 => recvr= <*ethapi.PublicNetAPI Value> fn= PublicNetAPI
		 => recvr= <*node.PrivateAdminAPI Value> fn= PrivateAdminAPI
		 => recvr= <*node.PublicAdminAPI Value> fn= PublicAdminAPI
		 => recvr= <*node.PublicAdminAPI Value> fn= PublicAdminAPI
		 => recvr= <*eth.PrivateAdminAPI Value> fn= PrivateAdminAPI
		*/
		reRec := regexp.MustCompile(fnRecName + `\s`)
		if !reRec.MatchString(cb.rcvr.String()) {
			continue
		}
		return fn
	}
	return nil
}

//func getAstType(astFile *ast.File, t reflect.Type) *ast.TypeSpec {
//	log.Println("getAstType", t.Name(), t.String())
//	for _, decl := range astFile.Decls {
//		d, ok := decl.(*ast.GenDecl)
//		if !ok {
//			continue
//		}
//		if d.Tok != token.TYPE {
//			continue
//		}
//		for _, s := range d.Specs {
//			sp, ok := s.(*ast.TypeSpec)
//			if !ok {
//				continue
//			}
//			if sp.Name != nil && sp.Name.Name == t.Name() {
//				return sp
//			} else if sp.Name != nil {
//				log.Println("nomatch", sp.Name.Name)
//			}
//		}
//
//	}
//	return nil
//}

func runtimeFuncName(rf *runtime.Func) string {
	spl := strings.Split(rf.Name(), ".")
	return spl[len(spl)-1]
}

func (d *OpenRPCDescription) findMethodByName(name string) (ok bool, method goopenrpcT.Method) {
	for _, m := range d.Doc.Methods {
		if m.Name == name {
			return true, m
		}
	}
	return false, goopenrpcT.Method{}
}

//func runtimeFuncPackageName(rf *runtime.Func) string {
//	re := regexp.MustCompile(`(?im)^(?P<pkgdir>.*/)(?P<pkgbase>[a-zA-Z0-9\-_]*)`)
//	match := re.FindStringSubmatch(rf.Name())
//	pmap := make(map[string]string)
//	for i, name := range re.SubexpNames() {
//		if i > 0 && i <= len(match) {
//			pmap[name] = match[i]
//		}
//	}
//	return pmap["pkgdir"] + pmap["pkgbase"]
//}