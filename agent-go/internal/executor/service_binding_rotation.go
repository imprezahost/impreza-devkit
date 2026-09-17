package executor

import (
	"context"
	"errors"
	"regexp"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var bindingRotationIDPattern = regexp.MustCompile(`^brot_[a-f0-9]{24}$`)

// The candidate is exactly the previous reference with a distinct generation
// revision; the 24-character login prefixes must never collide.
func rotationRefs(consumer string, intent *sdkclient.ServiceBindingRotationIntent) (serving, target sdkclient.ServiceBindingRef, err error) {
	invalid := errors.New("unsupported service binding rotation manifest")
	if intent == nil {
		return serving, target, invalid
	}
	if err = validateServiceBindingRef(consumer, intent.Previous); err != nil {
		return serving, target, err
	}
	if err = validateServiceBindingRef(consumer, intent.Candidate); err != nil {
		return serving, target, err
	}
	expected := intent.Previous
	expected.Revision = intent.Candidate.Revision
	if intent.Candidate != expected || intent.Previous.Revision[:24] == intent.Candidate.Revision[:24] {
		return serving, target, invalid
	}
	serving, target = intent.Candidate, intent.Previous
	if intent.Mode == "abandon" {
		serving, target = intent.Previous, intent.Candidate
	}
	return serving, target, nil
}

// Shared shape for the reviewed intent. The runtime variable check belongs only
// to the queued manifest: a resolved payload legitimately carries the injected
// serving credential.
func validateRotationShape(consumer string, runtime sdkclient.ManifestRuntime) (serving, target sdkclient.ServiceBindingRef, err error) {
	invalid := errors.New("unsupported service binding rotation manifest")
	intent := runtime.ServiceBindingRotation
	if intent == nil || runtime.ServiceBindingRotationProtocol != sdkclient.ServiceBindingRotationProtocol || runtime.ServiceBindingProtocol != sdkclient.ServiceBindingGenerationProtocol || runtime.Type != "docker-compose" || runtime.Build != nil || runtime.ServiceBindingRetirementProtocol != "" || len(runtime.ServiceBindingRetirements) != 0 || !bindingRotationIDPattern.MatchString(intent.RotationID) || (intent.Mode != "rotate" && intent.Mode != "abandon") {
		return serving, target, invalid
	}
	serving, target, err = rotationRefs(consumer, intent)
	if err != nil {
		return serving, target, err
	}
	if len(runtime.ServiceBindings) != 1 || runtime.ServiceBindings[0] != serving {
		return serving, target, invalid
	}
	return serving, target, nil
}

func validateServiceBindingRotation(p sdkclient.DeployPayload) error {
	serving, _, err := validateRotationShape(p.DeploymentID, p.Manifest.Runtime)
	if err != nil {
		return err
	}
	if _, exists := p.Vars[serving.Variable]; exists {
		return errors.New("service binding would overwrite a runtime variable")
	}
	return nil
}

// The queued payload cannot authorize a rotation retirement. Only the JIT
// response bound to this exact command can, and it never carries a password.
func (d *Docker) prepareServiceBindingRotation(ctx context.Context, cmd *sdkclient.PollCommand, p *sdkclient.DeployPayload) error {
	intent := p.Manifest.Runtime.ServiceBindingRotation
	_, target, err := rotationRefs(p.DeploymentID, intent)
	if err != nil {
		return err
	}
	fetch, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response, err := d.Client.AgentServiceBindingRetirements(fetch, p.DeploymentID, cmd.ID, cmd.ControlToken)
	if err != nil || response == nil || response.Protocol != sdkclient.ServiceBindingRotationProtocol || len(response.Retirements) != 1 || response.Retirements[0].ServiceBindingRef != target {
		return errors.New("rotation retirement authorization does not match the reviewed operation")
	}
	resolved := *p
	resolved.ServiceBindingRetirementAuthorizations = response.Retirements
	if err := validateResolvedRotation(resolved); err != nil {
		return err
	}
	p.ServiceBindingRetirementAuthorizations = response.Retirements
	return nil
}

func validateResolvedRotation(p sdkclient.DeployPayload) error {
	_, target, err := validateRotationShape(p.DeploymentID, p.Manifest.Runtime)
	if err != nil {
		return err
	}
	if len(p.ServiceBindingRetirementAuthorizations) != 1 || p.ServiceBindingRetirementAuthorizations[0].ServiceBindingRef != target {
		return errors.New("verified rotation retirement authorization is missing")
	}
	_, err = postgresGenerationRetireSQL(p.DeploymentID, p.ServiceBindingRetirementAuthorizations[0])
	return err
}

// Only a verified healthy replacement permits disabling the unused login. The
// serving login is never the retirement target, and database data is always
// retained; a failed retirement leaves an explicit pending outcome for review.
func (d *Docker) finishServiceBindingRotation(ctx context.Context, p sdkclient.DeployPayload, healthy bool) *sdkclient.ServiceBindingRotationResult {
	intent := p.Manifest.Runtime.ServiceBindingRotation
	if intent == nil {
		return nil
	}
	completed, pending := "completed", "cleanup_pending"
	if intent.Mode == "abandon" {
		completed, pending = "abandoned", "abandon_pending"
	}
	out := &sdkclient.ServiceBindingRotationResult{
		BindingID:         intent.Previous.BindingID,
		CandidateRevision: intent.Candidate.Revision,
		DataRetained:      true,
		PreviousRevision:  intent.Previous.Revision,
		RotationID:        intent.RotationID,
		Status:            pending,
	}
	if !healthy || validateResolvedRotation(p) != nil {
		return out
	}
	check, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if d.retirePostgresGeneration(check, p.DeploymentID, p.ServiceBindingRetirementAuthorizations[0]) == nil {
		out.Status = completed
	}
	return out
}
