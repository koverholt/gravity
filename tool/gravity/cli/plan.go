/*
Copyright 2018 Gravitational, Inc.

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

package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/gravitational/gravity/lib/constants"
	"github.com/gravitational/gravity/lib/defaults"
	"github.com/gravitational/gravity/lib/fsm"
	"github.com/gravitational/gravity/lib/localenv"
	"github.com/gravitational/gravity/lib/ops"
	"github.com/gravitational/gravity/lib/rpc"
	"github.com/gravitational/gravity/lib/storage"
	"github.com/gravitational/gravity/lib/update"
	"github.com/gravitational/gravity/lib/utils"

	"github.com/gravitational/trace"
)

func initOperationPlan(localEnv, updateEnv *localenv.LocalEnvironment) error {
	ctx := context.TODO()
	clusterEnv, err := localEnv.NewClusterEnvironment()
	if err != nil {
		return trace.Wrap(err)
	}

	if clusterEnv.Client == nil {
		return trace.BadParameter("this operation can only be executed on one of the master nodes")
	}

	secretsDir, err := fsm.AgentSecretsDir()
	if err != nil {
		return trace.Wrap(err)
	}
	creds, err := rpc.ClientCredentials(secretsDir)
	if err != nil {
		return trace.Wrap(err, "failed to load client RPC credentials from %v."+
			" Please make sure the upgrade operation has been started with `gravity upgrade`"+
			" or `gravity upgrade --manual` and retry.", secretsDir)
	}

	plan, err := update.InitOperationPlan(ctx, updateEnv, clusterEnv)
	if err != nil {
		return trace.Wrap(err)
	}

	err = update.SyncOperationPlanToCluster(ctx, *plan, creds)
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.Wrap(err)
}

func syncOperationPlan(localEnv *localenv.LocalEnvironment, updateEnv *localenv.LocalEnvironment) error {
	clusterEnv, err := localEnv.NewClusterEnvironment()
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.Wrap(update.SyncOperationPlan(clusterEnv.Backend, updateEnv.Backend))
}

func displayOperationPlan(localEnv, updateEnv *localenv.LocalEnvironment, format constants.Format) error {
	err := displayClusterOperationPlan(localEnv, format)
	if err != nil && !trace.IsNotFound(err) {
		log.Warnf("Failed to display the cluster operation plan: %v.", trace.DebugReport(err))
		// Fall-through to update/install operation plans
	}
	if err == nil {
		return nil
	}

	if hasUpdateOperation(updateEnv) {
		return trace.Wrap(displayUpdateOperationPlan(localEnv, updateEnv, format))
	}

	return trace.Wrap(displayInstallOperationPlan(format))
}

func displayClusterOperationPlan(env *localenv.LocalEnvironment, format constants.Format) error {
	operator, err := env.SiteOperator()
	if err != nil {
		return trace.Wrap(err)
	}

	cluster, err := operator.GetLocalSite()
	if err != nil {
		return trace.Wrap(err)
	}

	op, _, err := ops.GetLastOperation(cluster.Key(), operator)
	if err != nil {
		return trace.Wrap(err)
	}

	plan, err := operator.GetOperationPlan(op.Key())
	if err != nil {
		return trace.Wrap(err)
	}

	err = outputPlan(*plan, format)
	return trace.Wrap(err)
}

func displayUpdateOperationPlan(localEnv, updateEnv *localenv.LocalEnvironment, format constants.Format) error {
	clusterEnv, err := localEnv.NewClusterEnvironment()
	if err != nil {
		return trace.Wrap(err)
	}
	fsm, err := update.NewFSM(context.TODO(),
		update.FSMConfig{
			Backend:      clusterEnv.Backend,
			LocalBackend: updateEnv.Backend,
		})
	if err != nil {
		return trace.Wrap(err)
	}

	plan, err := fsm.GetPlan()
	if err != nil {
		return trace.Wrap(err)
	}
	err = outputPlan(*plan, format)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func displayInstallOperationPlan(format constants.Format) error {
	wizardEnv, err := localenv.NewRemoteEnvironment()
	if err != nil {
		return trace.Wrap(err)
	}
	if wizardEnv.Operator == nil {
		return trace.NotFound(`could not retrieve install operation plan.

If you have not launched the installation, or it has been started moments ago,
the plan may not be initialized yet.

If the install operation is in progress, please make sure you're invoking
"gravity plan" command from the same directory where "gravity install"
was run.`)
	}
	return trace.Wrap(displayInstallPlan(wizardEnv, format))
}

func displayInstallPlan(wizardEnv *localenv.RemoteEnvironment, format constants.Format) error {
	clusters, err := wizardEnv.Operator.GetSites(defaults.SystemAccountID)
	if err != nil {
		return trace.Wrap(err)
	}
	if len(clusters) != 1 {
		return trace.BadParameter("expected 1 cluster, got: %v", clusters)
	}
	op, _, err := ops.GetInstallOperation(clusters[0].Key(), wizardEnv.Operator)
	if err != nil {
		return trace.Wrap(err)
	}
	plan, err := wizardEnv.Operator.GetOperationPlan(op.Key())
	if err != nil {
		if trace.IsNotFound(err) {
			return trace.NotFound(
				"Install operation plan hasn't been initialized yet.")
		}
		return trace.Wrap(err)
	}
	err = outputPlan(*plan, format)
	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func outputPlan(plan storage.OperationPlan, format constants.Format) (err error) {
	switch format {
	case constants.EncodingYAML:
		err = fsm.FormatOperationPlanYAML(os.Stdout, plan)
	case constants.EncodingJSON:
		err = fsm.FormatOperationPlanJSON(os.Stdout, plan)
	case constants.EncodingText:
		fsm.FormatOperationPlanText(os.Stdout, plan)
		err = explainPlan(plan.Phases)
	default:
		return trace.BadParameter("unknown output format %q", format)
	}

	if err != nil {
		return trace.Wrap(err)
	}
	return nil
}

func explainPlan(phases []storage.OperationPhase) (err error) {
	for _, phase := range phases {
		if phase.State == storage.OperationPhaseStateFailed {
			if err := outputPhaseError(phase); err != nil {
				log.Warnf("Failed to output phase error: %v.", err)
			}
			return nil
		}
		if err := explainPlan(phase.Phases); err != nil {
			return trace.Wrap(err)
		}
	}
	return nil
}

func outputPhaseError(phase storage.OperationPhase) error {
	fmt.Printf("Phase %v (%v) failed.", phase.Description, phase.ID)
	if phase.Error != nil {
		var phaseErr trace.TraceErr
		if err := utils.UnmarshalError(phase.Error.Err, &phaseErr); err != nil {
			return trace.Wrap(err, "failed to unmarshal phase error from JSON")
		}
		fmt.Printf("\nError: %v\n", phaseErr.Err)
	}
	return nil
}

const recoveryModeWarning = "Failed to retrieve plan from etcd, showing cached plan. If etcd went down as a result of a system upgrade, you can perform a rollback phase. Run 'gravity plan --repair' when etcd connection is restored.\n"

// hasUpdateOperation returns true if there is an upgrade operation found
// in the backend used by the specified environment.
// updateEnv is the boltdb used for upgrades
func hasUpdateOperation(updateEnv *localenv.LocalEnvironment) bool {
	_, err := storage.GetLastOperation(updateEnv.Backend)
	return err == nil
}
