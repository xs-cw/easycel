package easycel

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
)

type Registry struct {
	nativeTypeProvider *nativeTypeProvider
	funcs              map[string][]cel.FunctionOpt
	variables          map[string]*cel.Type
	registry           ref.TypeRegistry
	adapter            ref.TypeAdapter
	provider           ref.TypeProvider
	tagName            string
	libraryName        string
}

type RegistryOption func(*Registry)

// WithTypeAdapter sets the type adapter used to convert types to CEL types.
func WithTypeAdapter(adapter ref.TypeAdapter) RegistryOption {
	return func(r *Registry) {
		r.adapter = adapter
	}
}

// WithTypeProvider sets the type provider used to convert types to CEL types.
func WithTypeProvider(provider ref.TypeProvider) RegistryOption {
	return func(r *Registry) {
		r.provider = provider
	}
}

// WithTagName sets the tag name used to convert types to CEL types.
func WithTagName(tagName string) RegistryOption {
	return func(r *Registry) {
		r.tagName = tagName
	}
}

// NewRegistry creates adapter new Registry.
func NewRegistry(libraryName string, opts ...RegistryOption) *Registry {
	r := &Registry{
		funcs:       make(map[string][]cel.FunctionOpt),
		variables:   make(map[string]*cel.Type),
		tagName:     "easycel",
		libraryName: libraryName,
	}
	for _, opt := range opts {
		opt(r)
	}
	registry, _ := types.NewRegistry()
	tp := newNativeTypeProvider(r.tagName, registry, registry)
	if r.adapter == nil {
		r.adapter = tp
	}
	if r.provider == nil {
		r.provider = tp
	}
	r.registry = registry
	r.nativeTypeProvider = tp

	return r
}

// LibraryName implements the Library interface method.
func (r *Registry) LibraryName() string {
	return r.libraryName
}

// CompileOptions implements the Library interface method.
func (r *Registry) CompileOptions() []cel.EnvOption {
	opts := []cel.EnvOption{}
	for name, fn := range r.funcs {
		opts = append(opts, cel.Function(name, fn...))
	}
	for name, typ := range r.variables {
		opts = append(opts, cel.Variable(name, typ))
	}
	opts = append(opts,
		cel.CustomTypeAdapter(r),
		cel.CustomTypeProvider(r),
	)
	return opts
}

// NativeToValue converts the input `value` to adapter CEL `ref.Val`.
func (r *Registry) NativeToValue(value any) ref.Val {
	return r.adapter.NativeToValue(value)
}

// EnumValue returns the numeric value of the given enum value name.
func (r *Registry) EnumValue(enumName string) ref.Val {
	return r.provider.EnumValue(enumName)
}

// FindIdent takes adapter qualified identifier name and returns adapter Value if one exists.
func (r *Registry) FindIdent(identName string) (ref.Val, bool) {
	return r.provider.FindIdent(identName)
}

// FindType looks up the Type given adapter qualified typeName.
func (r *Registry) FindType(typeName string) (*exprpb.Type, bool) {
	return r.provider.FindType(typeName)
}

// FindFieldType returns the field type for adapter checked type value.
func (r *Registry) FindFieldType(messageType string, fieldName string) (*ref.FieldType, bool) {
	return r.provider.FindFieldType(messageType, fieldName)
}

// NewValue creates adapter new type value from adapter qualified name and map of field name to value.
func (r *Registry) NewValue(typeName string, fields map[string]ref.Val) ref.Val {
	return r.provider.NewValue(typeName, fields)
}

// ProgramOptions implements the Library interface method.
func (r *Registry) ProgramOptions() []cel.ProgramOption {
	return []cel.ProgramOption{}
}

// RegisterType registers adapter type with the registry.
func (r *Registry) RegisterType(refTyes any) error {
	switch v := refTyes.(type) {
	case ref.Val:
		return r.registry.RegisterType(v.Type())
	case ref.Type:
		return r.registry.RegisterType(v)
	}
	return r.nativeTypeProvider.registerType(reflect.TypeOf(refTyes))
}

// RegisterVariable registers adapter value with the registry.
func (r *Registry) RegisterVariable(name string, val interface{}) error {
	if _, ok := r.variables[name]; ok {
		return fmt.Errorf("variable %s already registered", name)
	}
	typ := reflect.TypeOf(val)
	celType, ok := convertToCelType(typ)
	if !ok {
		return fmt.Errorf("variable %s type %s not supported", name, typ.String())
	}
	r.variables[name] = celType
	return nil
}

// RegisterFunction registers adapter function with the registry.
func (r *Registry) RegisterFunction(name string, fun interface{}) error {
	return r.registerFunction(name, fun, false)
}

// RegisterMethod registers adapter method with the registry.
func (r *Registry) RegisterMethod(name string, fun interface{}) error {
	return r.registerFunction(name, fun, true)
}

// RegisterConversion registers adapter conversion function with the registry.
func (r *Registry) RegisterConversion(fun any) error {
	return r.nativeTypeProvider.registerConversionsFunc(fun)
}

func (r *Registry) registerFunction(name string, fun interface{}, member bool) error {
	funVal := reflect.ValueOf(fun)
	if funVal.Kind() != reflect.Func {
		return fmt.Errorf("func must be func")
	}
	typ := funVal.Type()
	overloadOpt, err := r.getOverloadOpt(typ, funVal)
	if err != nil {
		return err
	}

	numIn := typ.NumIn()
	if member {
		if numIn == 0 {
			return fmt.Errorf("method must have at least one argument")
		}
	}
	argsCelType := make([]*cel.Type, 0, numIn)
	argsReflectType := make([]reflect.Type, 0, numIn)
	for i := 0; i < numIn; i++ {
		in := typ.In(i)
		celType, ok := convertToCelType(in)
		if !ok {
			return fmt.Errorf("invalid input type %s", in.String())
		}
		argsCelType = append(argsCelType, celType)
		argsReflectType = append(argsReflectType, in)
	}

	out := typ.Out(0)
	resultType, ok := convertToCelType(out)
	if !ok {
		return fmt.Errorf("invalid output type %s", out.String())
	}

	overloadID := formatFunction(name, argsReflectType, out, member)
	var funcOpt cel.FunctionOpt
	if member {
		funcOpt = cel.MemberOverload(overloadID, argsCelType, resultType, overloadOpt)
	} else {
		funcOpt = cel.Overload(overloadID, argsCelType, resultType, overloadOpt)
	}
	r.funcs[name] = append(r.funcs[name], funcOpt)
	return nil
}

func formatFunction(name string, args []reflect.Type, resultType reflect.Type, member bool) string {
	if member {
		return fmt.Sprintf("%s_member@%s_%s", name, formatTypes(args), typeName(resultType))
	}
	return fmt.Sprintf("%s_@%s_%s", name, formatTypes(args), typeName(resultType))
}

func formatTypes(types []reflect.Type) string {
	if len(types) == 0 {
		return ""
	}
	out := typeName(types[0])
	for _, typ := range types[1:] {
		out += "_" + typeName(typ)
	}
	return out
}

func (r *Registry) getOverloadOpt(typ reflect.Type, funVal reflect.Value) (out cel.OverloadOpt, err error) {
	numOut := typ.NumOut()
	switch numOut {
	default:
		return nil, fmt.Errorf("too many result")
	case 0:
		return nil, fmt.Errorf("result is required")
	case 2:
		if !typ.Out(1).AssignableTo(errorType) {
			return nil, fmt.Errorf("last result must be error %s", typ.String())
		}
	case 1:
	}

	numIn := typ.NumIn()
	isRefVal := make([]bool, numIn)
	isPtr := make([]bool, numIn)
	if numIn > 0 {
		for i := 0; i < numIn; i++ {
			in := typ.In(i)
			isRefVal[i] = in.Implements(refValType)
			isPtr[i] = in.Kind() == reflect.Ptr
		}
	}

	switch numIn {
	case 1:
		return cel.UnaryBinding(func(value ref.Val) ref.Val {
			val, err := reflectFuncCall(funVal,
				[]reflect.Value{
					convertToReflectValue(value, isRefVal[0], isPtr[0]),
				},
			)
			if err != nil {
				return types.WrapErr(err)
			}
			return r.NativeToValue(val.Interface())
		}), nil
	case 2:
		return cel.BinaryBinding(func(lhs ref.Val, rhs ref.Val) ref.Val {
			val, err := reflectFuncCall(funVal,
				[]reflect.Value{
					convertToReflectValue(lhs, isRefVal[0], isPtr[0]),
					convertToReflectValue(rhs, isRefVal[1], isPtr[1]),
				},
			)
			if err != nil {
				return types.WrapErr(err)
			}
			return r.NativeToValue(val.Interface())
		}), nil
	case 0:
		return cel.FunctionBinding(func(values ...ref.Val) ref.Val {
			val, err := reflectFuncCall(funVal, []reflect.Value{})
			if err != nil {
				return types.WrapErr(err)
			}
			return r.NativeToValue(val.Interface())
		}), nil
	default:
		return cel.FunctionBinding(func(values ...ref.Val) ref.Val {
			vals := make([]reflect.Value, 0, len(values))
			for i, value := range values {
				vals = append(vals,
					convertToReflectValue(value, isRefVal[i], isPtr[i]),
				)
			}
			val, err := reflectFuncCall(funVal, vals)
			if err != nil {
				return types.WrapErr(err)
			}
			return r.NativeToValue(val.Interface())
		}), nil
	}
}

func convertToReflectValue(val ref.Val, isRefVal, isPtr bool) reflect.Value {
	var value reflect.Value
	if isRefVal {
		value = reflect.ValueOf(val)
	} else {
		value = reflect.ValueOf(val.Value())
		if isPtr {
			if value.Kind() != reflect.Ptr {
				value = value.Addr()
			}
		} else {
			if value.Kind() == reflect.Ptr {
				value = value.Elem()
			}
		}
	}
	return value
}

func reflectFuncCall(funVal reflect.Value, values []reflect.Value) (reflect.Value, error) {
	results := funVal.Call(values)
	if len(results) == 2 {
		err, _ := results[1].Interface().(error)
		if err != nil {
			return reflect.Value{}, err
		}
	}
	return results[0], nil
}

func fieldNameWithTag(field reflect.StructField, tagName string) (name string, exported bool) {
	value, ok := field.Tag.Lookup(tagName)
	if !ok {
		return field.Name, true
	}

	name = strings.Split(value, ",")[0]
	if name == "-" {
		return "", false
	}

	if name == "" {
		name = field.Name
	}
	return name, true
}

func getFieldValue(adapter ref.TypeAdapter, refField reflect.Value) any {
	if refField.IsZero() {
		switch refField.Kind() {
		case reflect.Array, reflect.Slice:
			return types.NewDynamicList(adapter, []ref.Val{})
		case reflect.Map:
			return types.NewDynamicMap(adapter, map[ref.Val]ref.Val{})
		case reflect.Struct:
			if refField.Type() == timestampType {
				return types.Timestamp{Time: time.Unix(0, 0)}
			}
			return reflect.New(refField.Type()).Elem().Interface()
		case reflect.Pointer:
			return reflect.New(refField.Type().Elem()).Interface()
		}
	}
	return refField.Interface()
}
