/*
Copyright 2022 The KubeVela Authors.

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

package process

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/pkg/errors"

	"github.com/kubevela/workflow/pkg/cue/model"
)

// Context defines Rendering Context Interface
type Context interface {
	SetBase(base model.Instance) error
	AppendAuxiliaries(auxiliaries ...Auxiliary) error
	Output() (model.Instance, []Auxiliary)
	BaseContextFile() (string, error)
	ExtendedContextFile() (string, error)
	BaseContextLabels() map[string]string
	SetParameters(params map[string]interface{})
	PushData(key string, data interface{})
	GetCtx() context.Context
	SetCtx(context.Context)
}

// Auxiliary are objects rendered by definition template.
// the format for auxiliary resource is always: `outputs.<resourceName>`, it can be auxiliary workload or trait
type Auxiliary struct {
	Ins model.Instance
	// Type will be used to mark definition label for OAM runtime to get the CRD
	// It's now required for trait and main workload object. Extra workload CR object will not have the type.
	Type string

	// Workload or trait with multiple `outputs` will have a name, if name is empty, than it's the main of this type.
	Name string
}

type templateContext struct {
	// name is the component name of Application
	name           string
	workflowName   string
	publishVersion string
	configs        []map[string]string
	base           model.Instance
	auxiliaries    []Auxiliary
	// namespace is the namespace of Application which is used to set the namespace for Crossplane connection secret,
	// ComponentDefinition/TratiDefinition OpenAPI v3 schema
	namespace string
	// parameters is used to store the properties passed into the current component
	parameters map[string]interface{}
	// outputSecretName is used to store all secret names which are generated by cloud resource components
	outputSecretName string
	// requiredSecrets is used to store all secret names which are generated by cloud resource components and required by current component
	requiredSecrets []RequiredSecrets

	baseHooks      []BaseHook
	auxiliaryHooks []AuxiliaryHook

	data map[string]interface{}

	ctx context.Context
}

// RequiredSecrets is used to store all secret names which are generated by cloud resource components and required by current component
type RequiredSecrets struct {
	Namespace   string
	Name        string
	ContextName string
	Data        map[string]interface{}
}

// ContextData is the core data of process context
type ContextData struct {
	Name           string
	Namespace      string
	WorkflowName   string
	PublishVersion string

	Ctx            context.Context
	BaseHooks      []BaseHook
	AuxiliaryHooks []AuxiliaryHook
}

// NewContext create render templateContext
func NewContext(data ContextData) Context {
	ctx := &templateContext{
		namespace:      data.Namespace,
		name:           data.Name,
		workflowName:   data.WorkflowName,
		publishVersion: data.PublishVersion,

		configs:     []map[string]string{},
		auxiliaries: []Auxiliary{},
		parameters:  map[string]interface{}{},

		ctx:            data.Ctx,
		baseHooks:      data.BaseHooks,
		auxiliaryHooks: data.AuxiliaryHooks,
	}
	return ctx
}

// SetParameters sets templateContext parameters
func (ctx *templateContext) SetParameters(params map[string]interface{}) {
	ctx.parameters = params
}

// SetBase set templateContext base model
func (ctx *templateContext) SetBase(base model.Instance) error {
	for _, hook := range ctx.baseHooks {
		if err := hook.Exec(ctx, base); err != nil {
			return errors.Wrap(err, "cannot set base into context")
		}
	}
	ctx.base = base
	return nil
}

// AppendAuxiliaries add Assist model to templateContext
func (ctx *templateContext) AppendAuxiliaries(auxiliaries ...Auxiliary) error {
	for _, hook := range ctx.auxiliaryHooks {
		if err := hook.Exec(ctx, auxiliaries); err != nil {
			return errors.Wrap(err, "cannot append auxiliaries into context")
		}
	}
	ctx.auxiliaries = append(ctx.auxiliaries, auxiliaries...)
	return nil
}

// BaseContextFile return cue format string of templateContext
func (ctx *templateContext) BaseContextFile() (string, error) {
	var buff string
	buff += fmt.Sprintf(model.ContextName+": \"%s\"\n", ctx.name)
	buff += fmt.Sprintf(model.ContextNamespace+": \"%s\"\n", ctx.namespace)
	buff += fmt.Sprintf(model.ContextWorkflowName+": \"%s\"\n", ctx.workflowName)
	buff += fmt.Sprintf(model.ContextPublishVersion+": \"%s\"\n", ctx.publishVersion)

	if ctx.base != nil {
		buff += fmt.Sprintf(model.OutputFieldName+": %s\n", structMarshal(ctx.base.String()))
	}

	if len(ctx.auxiliaries) > 0 {
		var auxLines []string
		for _, auxiliary := range ctx.auxiliaries {
			auxLines = append(auxLines, fmt.Sprintf("\"%s\": %s", auxiliary.Name, structMarshal(auxiliary.Ins.String())))
		}
		if len(auxLines) > 0 {
			buff += fmt.Sprintf(model.OutputsFieldName+": {%s}\n", strings.Join(auxLines, "\n"))
		}
	}

	if len(ctx.configs) > 0 {
		bt, err := json.Marshal(ctx.configs)
		if err != nil {
			return "", err
		}
		buff += model.ConfigFieldName + ": " + string(bt) + "\n"
	}

	if len(ctx.requiredSecrets) > 0 {
		for _, s := range ctx.requiredSecrets {
			data, err := json.Marshal(s.Data)
			if err != nil {
				return "", err
			}
			buff += s.ContextName + ":" + string(data) + "\n"
		}
	}

	if ctx.parameters != nil {
		bt, err := json.Marshal(ctx.parameters)
		if err != nil {
			return "", err
		}
		buff += model.ParameterFieldName + ": " + string(bt) + "\n"
	}

	if ctx.outputSecretName != "" {
		buff += fmt.Sprintf("%s:\"%s\"", model.OutputSecretName, ctx.outputSecretName)
	}

	if ctx.data != nil {
		d, err := json.Marshal(ctx.data)
		if err != nil {
			return "", err
		}
		buff += fmt.Sprintf("\n %s", structMarshal(string(d)))
	}

	return fmt.Sprintf("context: %s", structMarshal(buff)), nil
}

// ExtendedContextFile return cue format string of templateContext and extended secret context
func (ctx *templateContext) ExtendedContextFile() (string, error) {
	context, err := ctx.BaseContextFile()
	if err != nil {
		return "", fmt.Errorf("failed to convert data to application with marshal err %w", err)
	}
	var bareSecret string
	if len(ctx.requiredSecrets) > 0 {
		for _, s := range ctx.requiredSecrets {
			data, err := json.Marshal(s.Data)
			if err != nil {
				return "", fmt.Errorf("failed to convert data %v to application with marshal err %w", data, err)
			}
			bareSecret += s.ContextName + ":" + string(data) + "\n"
		}
	}
	if bareSecret != "" {
		return context + "\n" + bareSecret, nil
	}
	return context, nil
}

func (ctx *templateContext) BaseContextLabels() map[string]string {
	return map[string]string{
		// name is oam.LabelAppComponent
		model.ContextName: ctx.name,
	}
}

// Output return model and auxiliaries of templateContext
func (ctx *templateContext) Output() (model.Instance, []Auxiliary) {
	return ctx.base, ctx.auxiliaries
}

// InsertSecrets will add cloud resource secret stuff to context
func (ctx *templateContext) InsertSecrets(outputSecretName string, requiredSecrets []RequiredSecrets) {
	if outputSecretName != "" {
		ctx.outputSecretName = outputSecretName
	}
	if requiredSecrets != nil {
		ctx.requiredSecrets = requiredSecrets
	}
}

// PushData appends arbitrary extension data to context
func (ctx *templateContext) PushData(key string, data interface{}) {
	if ctx.data == nil {
		ctx.data = map[string]interface{}{key: data}
		return
	}
	ctx.data[key] = data
}

func (ctx *templateContext) GetCtx() context.Context {
	if ctx.ctx != nil {
		return ctx.ctx
	}
	return context.TODO()
}

func (ctx *templateContext) SetCtx(newContext context.Context) {
	ctx.ctx = newContext
}

func structMarshal(v string) string {
	skip := false
	v = strings.TrimFunc(v, func(r rune) bool {
		if !skip {
			if unicode.IsSpace(r) {
				return true
			}
			skip = true

		}
		return false
	})

	if strings.HasPrefix(v, "{") {
		return v
	}
	return fmt.Sprintf("{%s}", v)
}
