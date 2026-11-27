package bytecode

import (
	"reflect"
	"testing"
	"unsafe"
)

// CopyWithoutVMCode copies every field of a Function but VMCode, which it
// leaves nil: each field is given a value other than its zero, and a field
// the copy does not carry -- one added to Function and not to the copy --
// fails by its name.
func TestCopyWithoutVMCode(t *testing.T) {
	var f Function
	v := reflect.ValueOf(&f).Elem()
	for i := 0; i < v.NumField(); i++ {
		fv, sf := v.Field(i), v.Type().Field(i)
		if !sf.IsExported() {
			t.Fatalf("field %s is unexported: CopyWithoutVMCode and this test copy exported fields", sf.Name)
		}
		if !setNonZero(fv) {
			t.Fatalf("field %s, of kind %s: give it a value other than its zero here", sf.Name, fv.Kind())
		}
	}
	marker := new(int)
	f.VMCode = unsafe.Pointer(marker)
	c := f.CopyWithoutVMCode()
	cv := reflect.ValueOf(c).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if name == "VMCode" {
			if c.VMCode != nil {
				t.Errorf("VMCode copied: want nil")
			}
			continue
		}
		if !reflect.DeepEqual(v.Field(i).Interface(), cv.Field(i).Interface()) {
			t.Errorf("field %s not copied by CopyWithoutVMCode", name)
		}
	}
}

// setNonZero sets v to a value other than its zero, reporting whether it
// knew how.
func setNonZero(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(3)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(3)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.UnsafePointer:
		v.SetPointer(unsafe.Pointer(new(int)))
	default:
		return false
	}
	return true
}
