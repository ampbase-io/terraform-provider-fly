package machines

import (
	"encoding/json"
	"fmt"
)

// UnmarshalJSON honours the dual-format behaviour documented on the
// `autostop` property in fly-machines.openapi3.json: the Machines API
// accepts and returns either the new string form ("off", "stop",
// "suspend") or the legacy boolean form (false → "off", true → "stop")
// for backward compatibility with older clients. The OpenAPI schema
// only declares the string form, so oapi-codegen generates a string
// type and decoding a response that uses the legacy bool form fails
// with "cannot unmarshal bool into Go struct field
// FlyMachineService.config.services.autostop of type
// machines.FlyMachineServiceAutostop".
//
// This hook restores the documented behaviour without patching the
// generated file (which would lose the fix on the next codegen).
func (a *FlyMachineServiceAutostop) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*a = FlyMachineServiceAutostop(s)
		return nil
	}
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		if b {
			*a = Stop
		} else {
			*a = Off
		}
		return nil
	}
	return fmt.Errorf("autostop: expected string or bool, got %s", string(data))
}
