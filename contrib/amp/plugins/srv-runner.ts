import type { BuiltinAgentMode, PluginAPI, PluginCommandContext, StatusItem, ThreadID } from '@ampcode/plugin'
import { chmod, mkdir, readFile, readdir, rename, stat, unlink, writeFile } from 'node:fs/promises'
import { homedir } from 'node:os'
import { basename, join } from 'node:path'

const stateDirectory = join(homedir(), '.local', 'state', 'amp-srv')
const ampSecretsPath = join(homedir(), '.local', 'share', 'amp', 'secrets.json')
const personalSkillsDirectory = join(homedir(), '.agents', 'skills')
const personalSkillNames = [
	'managing-nrc-tasks',
	'maintaining-room-memory',
	'querying-victorialogs-logsql',
	'searching-victorialogs',
	'using-mysqlsync-vm',
	'using-planner-cli',
	'using-sourcebot',
	'using-zoho-cli',
]
const personalBinaries = [
	{ source: join(homedir(), '.local', 'bin', 'nrc'), target: '/usr/local/bin/nrc' },
	{ source: join(homedir(), '.local', 'bin', 'sourcebot'), target: '/usr/local/bin/sourcebot' },
	{ source: join(homedir(), 'Code', 'planner_cli', 'planner'), target: '/usr/local/bin/planner' },
	{
		source: join(homedir(), 'Code', 'zoho_zeiterfassung', 'bin', 'zoho'),
		target: '/home/rene/Code/zoho_zeiterfassung/bin/zoho',
	},
]
const personalConfigs = [
	{ source: join(homedir(), '.config', 'nrc', 'config.yaml'), target: '/root/.config/nrc/config.yaml' },
	{ source: join(homedir(), '.config', 'planner-cli', 'config.json'), target: '/root/.config/planner-cli/config.json' },
	{ source: join(homedir(), '.config', 'planner-cli', 'msal_cache.json'), target: '/root/.config/planner-cli/msal_cache.json' },
	{ source: join(homedir(), 'Code', 'zoho_zeiterfassung', 'bin', '.env'), target: '/home/rene/Code/zoho_zeiterfassung/bin/.env' },
]
const guestSSHOptions = ['-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=accept-new', '-o', 'ConnectTimeout=5']
const trustedGuestSSHOptions = ['-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=5']
const pushTimeoutMs = 120_000
const personalProfileConsentMessage =
	'The persistent VM will receive your Amp credential, NRC config, Planner OAuth cache, and Zoho OAuth environment, plus the selected skills and CLI binaries. Secret files will be root-only and are removed when the VM is deleted.'

interface ManagedInstance {
	instance: string
	runnerID: string
	repository: string
	branch: string
	createdAt: string
	status: 'provisioning' | 'ready' | 'failed'
	threadID?: ThreadID
	error?: string
}

interface ProcessOptions {
	cwd?: string
	stdin?: string | Uint8Array
	allowFailure?: boolean
	timeoutMs?: number
}

interface ProcessResult {
	stdout: string
	stderr: string
	exitCode: number
	timedOut: boolean
}

interface BinaryProcessResult {
	stdout: Uint8Array
	stderr: string
	exitCode: number
}

interface ProgressDisplay {
	update(step: number, label: string): void
	dispose(): void
}

export default function (amp: PluginAPI) {
	amp.logger.log('srv runner plugin initialized')

	amp.registerCommand(
		'srv-runner.new-isolated-thread',
		{
			title: 'New isolated thread',
			category: 'srv Runner',
			description: 'Create a fresh srv VM and start an Amp thread on its dedicated runner.',
		},
		async (ctx) => {
			await createIsolatedThread(amp, ctx)
		},
	)

	amp.registerCommand(
		'srv-runner.show-current-vm',
		{
			title: 'Show current thread VM',
			category: 'srv Runner',
			description: 'Show the srv VM associated with the active Amp thread.',
		},
		async (ctx) => {
			const managed = await managedThreadForContext(ctx)
			if (!managed) return

			const result = await runProcess(['ssh', 'srv', '--', '--json', 'inspect', managed.instance], {
				allowFailure: true,
			})
			if (result.exitCode !== 0) {
				await ctx.ui.notify(`srv Runner: inspect failed for ${managed.instance}:\n${failureMessage(result)}`)
				return
			}

			const details = parseInstanceSummary(result.stdout)
			await ctx.ui.notify(
				[
					`VM: ${managed.instance}`,
					`Runner: ${managed.runnerID}`,
					`Repository: ${managed.repository}`,
					`Branch: ${managed.branch}`,
					...details,
				].join('\n'),
			)
		},
	)

	amp.registerCommand(
		'srv-runner.list-managed-vms',
		{
			title: 'List managed VMs',
			category: 'srv Runner',
			description: 'List srv VMs created by this plugin, including failed provisioning attempts.',
		},
		async (ctx) => {
			const instances = await readInstances()
			if (instances.length === 0) {
				await ctx.ui.notify('srv Runner: no managed VMs.')
				return
			}
			await ctx.ui.notify(
				instances
					.map((managed) =>
						[
							`${managed.instance}: ${managed.status}`,
							`  thread: ${managed.threadID ?? 'none'}`,
							`  repository: ${managed.repository} (${managed.branch})`,
							...(managed.error ? [`  error: ${managed.error}`] : []),
						].join('\n'),
					)
					.join('\n\n'),
			)
		},
	)

	amp.registerCommand(
		'srv-runner.delete-managed-vm',
		{
			title: 'Delete managed VM…',
			category: 'srv Runner',
			description: 'Select and permanently delete any srv VM created by this plugin.',
		},
		async (ctx) => {
			const instances = await readInstances()
			if (instances.length === 0) {
				await ctx.ui.notify('srv Runner: no managed VMs.')
				return
			}
			const selected = await ctx.ui.select({
				title: 'Delete managed srv VM',
				options: instances.map((managed) => `${managed.instance} (${managed.status})`),
			})
			if (!selected) return
			const instance = selected.slice(0, selected.indexOf(' ('))
			const managed = instances.find((candidate) => candidate.instance === instance)
			if (!managed) return

			const confirmed = await ctx.ui.confirm({
				title: `Permanently delete ${instance}?`,
				message: 'The VM filesystem and all unpushed changes will be irreversibly deleted.',
				confirmButtonText: 'Delete VM',
			})
			if (!confirmed) return

			await runProcess(['ssh', 'srv', 'delete', instance])
			await deleteInstanceRecord(instance)
			await ctx.ui.notify(`srv Runner: deleted ${instance}.`)
		},
	)

	amp.registerCommand(
		'srv-runner.retry-failed-vm',
		{
			title: 'Retry failed VM…',
			category: 'srv Runner',
			description: 'Resume provisioning a failed srv VM and create its Amp thread.',
		},
		async (ctx) => {
			await retryFailedVM(amp, ctx)
		},
	)

	amp.registerCommand(
		'srv-runner.start-current-vm',
		{
			title: 'Start current thread VM',
			category: 'srv Runner',
			description: 'Start the srv VM associated with the active Amp thread.',
		},
		async (ctx) => {
			const managed = await managedThreadForContext(ctx)
			if (!managed) return

			const progress = createProgressDisplay(amp, managed.instance, 2)
			try {
				progress.update(1, 'Starting VM')
				await runProcess(['ssh', 'srv', 'start', managed.instance])
				await waitForInstance(managed.instance, (seconds, state) => {
					progress.update(2, `Waiting for ready · ${seconds}s · ${state}`)
				})
				await ctx.ui.notify(`✓ srv Runner: ${managed.instance} is ready; its Amp runner will reconnect automatically.`)
			} finally {
				progress.dispose()
			}
		},
	)

	amp.registerCommand(
		'srv-runner.push-current-vm-branch',
		{
			title: 'Push current VM branch…',
			category: 'srv Runner',
			description: 'Temporarily forward the local SSH agent and push the current VM branch.',
		},
		async (ctx) => {
			await pushCurrentVMBranch(ctx)
		},
	)

	amp.registerCommand(
		'srv-runner.stop-current-vm',
		{
			title: 'Stop current thread VM',
			category: 'srv Runner',
			description: 'Stop the srv VM while preserving its root filesystem and checkout.',
		},
		async (ctx) => {
			const managed = await managedThreadForContext(ctx)
			if (!managed) return

			const confirmed = await ctx.ui.confirm({
				title: `Stop ${managed.instance}?`,
				message: 'The Amp runner will go offline. The VM filesystem and unpushed changes will be preserved.',
				confirmButtonText: 'Stop VM',
			})
			if (!confirmed) return

			await runProcess(['ssh', 'srv', 'stop', managed.instance])
			await ctx.ui.notify(`srv Runner: stopped ${managed.instance}.`)
		},
	)

	amp.registerCommand(
		'srv-runner.delete-current-vm',
		{
			title: 'Delete current thread VM',
			category: 'srv Runner',
			description: 'Permanently delete the srv VM and its checkout.',
		},
		async (ctx) => {
			const managed = await managedThreadForContext(ctx)
			if (!managed || !ctx.thread) return

			const confirmed = await ctx.ui.confirm({
				title: `Permanently delete ${managed.instance}?`,
				message: 'The VM filesystem and all unpushed changes will be irreversibly deleted.',
				confirmButtonText: 'Delete VM',
			})
			if (!confirmed) return

			await runProcess(['ssh', 'srv', 'delete', managed.instance])
			await deleteInstanceRecord(managed.instance)
			await ctx.ui.notify(`srv Runner: deleted ${managed.instance}.`)
		},
	)
}

async function createIsolatedThread(amp: PluginAPI, ctx: PluginCommandContext) {
	const workspaceURI = ctx.system.workspaceRoot
	if (!workspaceURI) {
		await ctx.ui.notify('srv Runner: open a Git repository before creating an isolated thread.')
		return
	}

	const workspaceRoot = amp.helpers.filePathFromURI(workspaceURI)
	const repositoryResult = await runProcess(['git', 'config', '--get', 'remote.origin.url'], {
		cwd: workspaceRoot,
		allowFailure: true,
	})
	const repository = repositoryResult.stdout.trim()
	if (!repository) {
		await ctx.ui.notify('srv Runner: the current repository has no origin remote.')
		return
	}

	const currentBranch = (await runProcess(['git', 'branch', '--show-current'], { cwd: workspaceRoot })).stdout.trim()
	if (!currentBranch) {
		await ctx.ui.notify('srv Runner: the current checkout is detached; select a branch locally first.')
		return
	}
	const remoteBranch = await runProcess(
		['git', 'ls-remote', '--exit-code', 'origin', `refs/heads/${currentBranch}`],
		{ cwd: workspaceRoot, allowFailure: true },
	)
	if (remoteBranch.exitCode !== 0) {
		await ctx.ui.notify(`srv Runner: branch ${currentBranch} does not exist on origin or origin is not accessible.`)
		return
	}
	if (!globalThis.process.env.SSH_AUTH_SOCK && isSSHRepository(repository)) {
		await ctx.ui.notify('srv Runner: SSH_AUTH_SOCK is not set; start or attach an SSH agent before cloning this repository.')
		return
	}

	const status = (await runProcess(['git', 'status', '--porcelain=v1'], { cwd: workspaceRoot })).stdout.trim()
	if (status) {
		const continueWithoutChanges = await ctx.ui.confirm({
			title: 'Local changes will not be copied',
			message: 'The srv VM receives a fresh clone from origin. Uncommitted and untracked local files are not included.',
			confirmButtonText: 'Continue',
		})
		if (!continueWithoutChanges) return
	}

	const prompt = await ctx.ui.input({
		title: 'Task for the isolated Amp thread',
		helpText: `Repository: ${repository}\nBranch: ${currentBranch}`,
		submitButtonText: 'Continue',
	})
	if (!prompt?.trim()) return

	const modeSelection = await ctx.ui.select({
		title: 'Amp mode',
		options: ['medium', 'low', 'high', 'ultra'],
		initialValue: 'medium',
	})
	if (!modeSelection) return
	const mode = modeSelection as BuiltinAgentMode

	const size = await ctx.ui.select({
		title: 'srv VM size',
		message: 'CPU / memory / root filesystem',
		options: ['4 CPU / 8 GiB / 30 GiB', '2 CPU / 4 GiB / 20 GiB', '8 CPU / 16 GiB / 40 GiB'],
		initialValue: '4 CPU / 8 GiB / 30 GiB',
	})
	if (!size) return
	const resources = resourcesForSize(size)

	try {
		await validatePersonalProfileSources()
	} catch (error) {
		await ctx.ui.notify(`srv Runner: personal profile is incomplete:\n${errorMessage(error)}`)
		return
	}

	const copyCredential = await ctx.ui.confirm({
		title: 'Copy credentials and personal tools into the VM?',
		message: personalProfileConsentMessage,
		confirmButtonText: 'Create VM',
	})
	if (!copyCredential) return

	let secrets: Uint8Array
	try {
		secrets = await readFile(ampSecretsPath)
	} catch {
		await ctx.ui.notify(`srv Runner: cannot read Amp authentication from ${ampSecretsPath}. Run amp login first.`)
		return
	}

	const instance = instanceName(repository)
	let managed: ManagedInstance = {
		instance,
		runnerID: instance,
		repository,
		branch: currentBranch,
		createdAt: new Date().toISOString(),
		status: 'provisioning',
	}
	await writeInstance(managed)
	const parentThreadID = ctx.thread?.id
	await ctx.ui.notify(`srv Runner: provisioning ${instance} in the background. Follow progress in the status bar.`)
	setTimeout(() => {
		void provisionNewVM(amp, managed, resources, secrets, mode, prompt.trim(), parentThreadID)
	}, 0)
}

async function retryFailedVM(amp: PluginAPI, ctx: PluginCommandContext) {
	const failedInstances = (await readInstances()).filter((managed) => managed.status === 'failed')
	if (failedInstances.length === 0) {
		await ctx.ui.notify('srv Runner: no failed VMs to retry.')
		return
	}

	const selected = await ctx.ui.select({
		title: 'Retry failed srv VM',
		options: failedInstances.map((managed) => managed.instance),
	})
	if (!selected) return
	let managed = failedInstances.find((candidate) => candidate.instance === selected)
	if (!managed) return

	const prompt = await ctx.ui.input({
		title: 'Task for the isolated Amp thread',
		helpText: `Repository: ${managed.repository}\nBranch: ${managed.branch}`,
		submitButtonText: 'Retry',
	})
	if (!prompt?.trim()) return

	const modeSelection = await ctx.ui.select({
		title: 'Amp mode',
		options: ['medium', 'low', 'high', 'ultra'],
		initialValue: 'medium',
	})
	if (!modeSelection) return
	try {
		await validatePersonalProfileSources()
	} catch (error) {
		await ctx.ui.notify(`srv Runner: personal profile is incomplete:\n${errorMessage(error)}`)
		return
	}
	const copyCredential = await ctx.ui.confirm({
		title: 'Copy credentials and personal tools into the VM?',
		message: personalProfileConsentMessage,
		confirmButtonText: 'Retry provisioning',
	})
	if (!copyCredential) return

	let secrets: Uint8Array
	try {
		secrets = await readFile(ampSecretsPath)
	} catch {
		await ctx.ui.notify(`srv Runner: cannot read Amp authentication from ${ampSecretsPath}. Run amp login first.`)
		return
	}

	managed = { ...managed, status: 'provisioning', error: undefined }
	await writeInstance(managed)
	const parentThreadID = ctx.thread?.id
	await ctx.ui.notify(`srv Runner: retrying ${managed.instance} in the background. Follow progress in the status bar.`)
	setTimeout(() => {
		void provisionExistingVM(
			amp,
			managed,
			secrets,
			modeSelection as BuiltinAgentMode,
			prompt.trim(),
			parentThreadID,
		)
	}, 0)
}

async function provisionNewVM(
	amp: PluginAPI,
	initialManaged: ManagedInstance,
	resources: ReturnType<typeof resourcesForSize>,
	secrets: Uint8Array,
	mode: BuiltinAgentMode,
	prompt: string,
	parentThreadID?: ThreadID,
) {
	let managed = initialManaged
	const { instance, repository, branch } = managed
	const progress = createProgressDisplay(amp, instance, 9)

	try {
		progress.update(1, 'Creating VM')
		await runProcess([
			'ssh', 'srv', 'new', instance,
			'--cpus', String(resources.cpus),
			'--ram', `${resources.ramGiB}G`,
			'--rootfs-size', `${resources.diskGiB}G`,
		])
		await waitForInstance(instance, (seconds, state) => progress.update(2, `Booting VM · ${seconds}s · ${state}`))
		await waitForGuestSSH(instance, (seconds) => progress.update(3, `Waiting for SSH · ${seconds}s`))
		progress.update(4, 'Copying Amp credential')
		await copyAmpCredential(instance, secrets)
		progress.update(5, 'Installing personal tools and skills')
		await installPersonalProfile(instance)
		progress.update(6, 'Installing Amp and preparing repository')
		const proxyURL = await ampConnectGatewayURL(instance)
		await bootstrapRunner(instance, repository, branch, proxyURL)
		await waitForRunnerRegistration(instance, (seconds) => progress.update(7, `Waiting for Amp runner · ${seconds}s`))
		progress.update(8, 'Creating Amp thread')
		const thread = await createRunnerThread(amp.getBuiltinAgent(mode), instance, parentThreadID)
		managed = { ...managed, status: 'ready', threadID: thread.id }
		await writeInstance(managed)
		progress.update(9, 'Starting task')
		await thread.appendUserMessage({ type: 'user-message', content: prompt })
		await amp.ui.notify(`✓ srv Runner: thread ${thread.id} is running on ${instance}.`)
	} catch (error) {
		managed = { ...managed, status: 'failed', error: errorMessage(error).slice(-2_000) }
		try {
			await writeInstance(managed)
		} catch {
			// The VM name remains in the notification even if local state persistence fails.
		}
		await amp.ui.notify(
			`✗ srv Runner: provisioning failed for ${instance}. The VM was preserved for inspection.\n${errorMessage(error)}`,
		)
	} finally {
		progress.dispose()
	}
}

async function provisionExistingVM(
	amp: PluginAPI,
	initialManaged: ManagedInstance,
	secrets: Uint8Array,
	mode: BuiltinAgentMode,
	prompt: string,
	parentThreadID?: ThreadID,
) {
	let managed = initialManaged
	const progress = createProgressDisplay(amp, managed.instance, 8)

	try {
		await waitForInstance(managed.instance, (seconds, state) => progress.update(1, `Waiting for VM · ${seconds}s · ${state}`))
		await waitForGuestSSH(managed.instance, (seconds) => progress.update(2, `Waiting for SSH · ${seconds}s`))
		progress.update(3, 'Copying Amp credential')
		await copyAmpCredential(managed.instance, secrets)
		progress.update(4, 'Installing personal tools and skills')
		await installPersonalProfile(managed.instance)
		progress.update(5, 'Installing Amp and preparing repository')
		const proxyURL = await ampConnectGatewayURL(managed.instance)
		await bootstrapRunner(managed.instance, managed.repository, managed.branch, proxyURL)
		await waitForRunnerRegistration(managed.instance, (seconds) => progress.update(6, `Waiting for Amp runner · ${seconds}s`))
		progress.update(7, 'Creating Amp thread')
		const thread = await createRunnerThread(amp.getBuiltinAgent(mode), managed.runnerID, parentThreadID)
		managed = { ...managed, status: 'ready', threadID: thread.id }
		await writeInstance(managed)
		progress.update(8, 'Starting task')
		await thread.appendUserMessage({ type: 'user-message', content: prompt })
		await amp.ui.notify(`✓ srv Runner: thread ${thread.id} is running on ${managed.instance}.`)
	} catch (error) {
		managed = { ...managed, status: 'failed', error: errorMessage(error).slice(-2_000) }
		await writeInstance(managed)
		await amp.ui.notify(`✗ srv Runner: retry failed for ${managed.instance}.\n${errorMessage(error)}`)
	} finally {
		progress.dispose()
	}
}

async function copyAmpCredential(instance: string, secrets: Uint8Array) {
	await runProcess(
		[
			'ssh',
			...guestSSHOptions,
			`root@${instance}`,
			'set -eu; umask 077; install -d -m 0700 "$HOME/.local/share/amp"; cat > "$HOME/.local/share/amp/secrets.json"; chmod 0600 "$HOME/.local/share/amp/secrets.json"',
		],
		{ stdin: secrets },
	)
}

async function validatePersonalProfileSources() {
	const sources = [
		...personalSkillNames.map((name) => join(personalSkillsDirectory, name)),
		...personalBinaries.map(({ source }) => source),
		...personalConfigs.map(({ source }) => source),
	]
	for (const source of sources) {
		try {
			await stat(source)
		} catch {
			throw new Error(`missing ${source}`)
		}
	}
}

async function installPersonalProfile(instance: string) {
	await validatePersonalProfileSources()
	const skills = await runBinaryProcess([
		'tar',
		'-chf',
		'-',
		'-C',
		personalSkillsDirectory,
		...personalSkillNames,
	])
	await runProcess(
		[
			'ssh',
			...guestSSHOptions,
			`root@${instance}`,
			'install -d -m 0755 /root/.agents/skills; tar --no-same-owner -xf - -C /root/.agents/skills',
		],
		{ stdin: skills.stdout },
	)

	for (const binary of personalBinaries) {
		await copyGuestFile(instance, binary.source, binary.target, 0o755, 0o755)
	}
	for (const config of personalConfigs) {
		await copyGuestFile(instance, config.source, config.target, 0o600, 0o700)
	}
	await runProcess([
		'ssh',
		...guestSSHOptions,
		`root@${instance}`,
		'ln -sfn /home/rene/Code/zoho_zeiterfassung/bin/zoho /usr/local/bin/zoho',
	])
}

async function copyGuestFile(instance: string, source: string, target: string, mode: number, directoryMode: number) {
	const contents = await readFile(source)
	const targetArgument = base64(target)
	const command =
		`target=$(printf '%s' '${targetArgument}' | base64 -d); ` +
		`install -d -m ${directoryMode.toString(8)} "$(dirname -- "$target")"; ` +
		`cat > "$target"; chmod ${mode.toString(8)} "$target"`
	await runProcess(['ssh', ...guestSSHOptions, `root@${instance}`, command], { stdin: contents })
}

async function ampConnectGatewayURL(instance: string): Promise<string> {
	const result = await runProcess(['ssh', 'srv', '--', '--json', 'inspect', instance])
	const parsed = JSON.parse(result.stdout) as { instance?: { amp_connect_gateway?: string } }
	const proxyURL = parsed.instance?.amp_connect_gateway?.trim()
	if (!proxyURL) {
		throw new Error(`srv did not expose an Amp CONNECT gateway for ${instance}`)
	}
	return proxyURL
}

async function bootstrapRunner(instance: string, repository: string, branch: string, proxyURL: string) {
	const script = `set -euo pipefail
repository=$(printf '%s' "$1" | base64 -d)
branch=$(printf '%s' "$2" | base64 -d)
runner_id=$(printf '%s' "$3" | base64 -d)
proxy_url=$(printf '%s' "$4" | base64 -d)

packages=()
command -v curl >/dev/null 2>&1 || packages+=(curl)
command -v ssh >/dev/null 2>&1 || packages+=(openssh)
command -v jq >/dev/null 2>&1 || packages+=(jq)
command -v tmux >/dev/null 2>&1 || packages+=(tmux)
if (( \${#packages[@]} > 0 )); then
  pacman -Sy --noconfirm --needed "\${packages[@]}"
fi

curl -fsSL https://ampcode.com/install.sh | bash

install -d -m 0755 /workspace
export GIT_SSH_COMMAND='ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new'
if [[ ! -e /workspace/repository ]]; then
  git clone --branch "$branch" --single-branch -- "$repository" /workspace/repository
elif [[ ! -d /workspace/repository/.git ]]; then
  echo '/workspace/repository exists but is not a Git checkout; refusing to overwrite it' >&2
  exit 1
fi

cd /workspace/repository
git submodule update --init --recursive

if [[ -x .agents/setup ]]; then
  ./.agents/setup
fi

gateway_ip=$(ip -4 route show default | awk '/^default / { print $3; exit }')
tailnet_suffix=$(tailscale status --json | jq -r '.Self.DNSName // empty' | cut -d. -f2- | sed 's/\.$//')
no_proxy="localhost,127.0.0.1,::1,169.254.169.254,100.64.0.0/10,monitoring,sourcebot,$gateway_ip"
if [[ -n "$tailnet_suffix" ]]; then
  no_proxy="$no_proxy,.$tailnet_suffix"
fi

cat > /etc/systemd/system/amp-runner.service <<EOF
[Unit]
Description=Amp runner for $runner_id
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=simple
User=root
Environment=HOME=/root
Environment="HTTP_PROXY=$proxy_url"
Environment="HTTPS_PROXY=$proxy_url"
Environment="http_proxy=$proxy_url"
Environment="https_proxy=$proxy_url"
Environment="NO_PROXY=$no_proxy"
Environment="no_proxy=$no_proxy"
WorkingDirectory=/workspace/repository
ExecStart=/root/.local/bin/amp --no-tui --runner-id $runner_id --remote-control-terminal
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable amp-runner.service
systemctl restart amp-runner.service
systemctl is-active --quiet amp-runner.service
`

	await runProcess(
		[
			'ssh',
			'-A',
			...guestSSHOptions,
			`root@${instance}`,
			`bash -s -- ${base64(repository)} ${base64(branch)} ${base64(instance)} ${base64(proxyURL)}`,
		],
		{ stdin: script },
	)
}

async function createRunnerThread(
	agent: ReturnType<PluginAPI['getBuiltinAgent']>,
	runnerID: string,
	parentThreadID?: ThreadID,
) {
	let lastError: unknown
	for (let attempt = 0; attempt < 12; attempt++) {
		try {
			return await agent.createThread({
				show: true,
				parentThreadID,
				executor: { type: 'runner', id: runnerID },
			})
		} catch (error) {
			lastError = error
			await sleep(Math.min(1_000 + attempt * 500, 5_000))
		}
	}
	throw new Error(`Amp runner ${runnerID} did not become available: ${errorMessage(lastError)}`)
}

async function waitForInstance(instance: string, onWait?: (seconds: number, state: string) => void) {
	let lastError = ''
	const startedAt = Date.now()
	for (let attempt = 0; attempt < 90; attempt++) {
		const result = await runProcess(['ssh', 'srv', '--', '--json', 'inspect', instance], { allowFailure: true })
		if (result.exitCode === 0) {
			try {
				const parsed = JSON.parse(result.stdout) as { instance?: { state?: string }; state?: string }
				const state = parsed.instance?.state ?? parsed.state
				if (state === 'ready') return
				if (state === 'failed') throw new Error(`srv VM ${instance} entered failed state`)
				lastError = `state=${state ?? 'unknown'}`
				onWait?.(elapsedSeconds(startedAt), state ?? 'unknown')
			} catch (error) {
				if (error instanceof SyntaxError) {
					lastError = 'invalid inspect JSON'
					onWait?.(elapsedSeconds(startedAt), 'invalid response')
				}
				else throw error
			}
		} else {
			lastError = failureMessage(result)
			onWait?.(elapsedSeconds(startedAt), 'unreachable')
		}
		await sleep(1_000)
	}
	throw new Error(`Timed out waiting for srv VM ${instance} (${lastError})`)
}

async function waitForGuestSSH(instance: string, onWait?: (seconds: number) => void) {
	let lastError = ''
	const startedAt = Date.now()
	for (let attempt = 0; attempt < 45; attempt++) {
		const result = await runProcess(['ssh', ...guestSSHOptions, `root@${instance}`, 'true'], { allowFailure: true })
		if (result.exitCode === 0) return
		lastError = failureMessage(result)
		onWait?.(elapsedSeconds(startedAt))
		await sleep(1_000)
	}
	throw new Error(`Timed out waiting for SSH on ${instance} (${lastError})`)
}

async function waitForRunnerRegistration(instance: string, onWait?: (seconds: number) => void) {
	const registrationCommand =
		`since=$(systemctl show amp-runner.service -p ActiveEnterTimestamp --value); ` +
		`journalctl -u amp-runner.service --since "$since" --no-pager | grep -Fq 'Registered.'`
	const startedAt = Date.now()
	for (let attempt = 0; attempt < 60; attempt++) {
		const result = await runProcess(
			['ssh', ...guestSSHOptions, `root@${instance}`, registrationCommand],
			{ allowFailure: true },
		)
		if (result.exitCode === 0) return
		onWait?.(elapsedSeconds(startedAt))
		await sleep(1_000)
	}
	const journal = await runProcess(
		[
			'ssh',
			...guestSSHOptions,
			`root@${instance}`,
			'journalctl -u amp-runner.service --since "2 minutes ago" --no-pager -n 30',
		],
		{ allowFailure: true },
	)
	throw new Error(`Amp runner ${instance} did not register:\n${failureMessage(journal)}`)
}

async function pushCurrentVMBranch(ctx: PluginCommandContext) {
	const managed = await managedThreadForContext(ctx)
	if (!managed) return
	if (!isSSHRepository(managed.repository)) {
		await ctx.ui.notify(`srv Runner: ${managed.repository} is not an SSH Git remote.`)
		return
	}
	if (!globalThis.process.env.SSH_AUTH_SOCK) {
		await ctx.ui.notify('srv Runner: SSH_AUTH_SOCK is not set; start or attach an SSH agent before pushing.')
		return
	}
	const agentResult = await runProcess(['ssh-add', '-l'], { allowFailure: true })
	if (agentResult.exitCode !== 0) {
		await ctx.ui.notify(`srv Runner: the local SSH agent has no available identity:\n${failureMessage(agentResult)}`)
		return
	}

	const revisionScript = `set -euo pipefail
cd /workspace/repository
branch=$(git branch --show-current)
[[ -n "$branch" ]]
git check-ref-format --branch "$branch" >/dev/null
oid=$(git rev-parse --verify 'HEAD^{commit}')
printf '%s\n%s\n' "$branch" "$oid"
`
	const revisionResult = await runProcess(
		[
			'ssh',
			...trustedGuestSSHOptions,
			'-o', 'ControlMaster=no',
			'-o', 'ControlPath=none',
			`root@${managed.instance}`,
			'bash -s',
		],
		{ stdin: revisionScript, allowFailure: true },
	)
	if (revisionResult.exitCode !== 0) {
		await ctx.ui.notify(`srv Runner: cannot determine the current revision on ${managed.instance}:\n${failureMessage(revisionResult)}`)
		return
	}
	const [branch, oid, ...unexpectedOutput] = revisionResult.stdout.trim().split('\n')
	if (!branch || !oid || unexpectedOutput.length > 0 || !/^[0-9a-f]{40,64}$/.test(oid)) {
		await ctx.ui.notify(`srv Runner: ${managed.instance} returned an invalid Git revision.`)
		return
	}

	const confirmed = await ctx.ui.confirm({
		title: `Push ${branch} from ${managed.instance}?`,
		message:
			`Temporarily expose your local SSH agent to ${managed.instance} and push:\n\n` +
			`${managed.repository}\n${branch} @ ${oid.slice(0, 12)} → ${branch}\n\n` +
			'The agent is available inside the VM only until this push command finishes.',
		confirmButtonText: 'Push branch',
	})
	if (!confirmed) return

	const script = `set -euo pipefail
repository=$(printf '%s' "$1" | base64 -d)
expected_branch=$(printf '%s' "$2" | base64 -d)
expected_oid=$(printf '%s' "$3" | base64 -d)
cd /workspace/repository
current_branch=$(git branch --show-current)
if [[ -z "$current_branch" ]]; then
  echo 'cannot push from a detached HEAD' >&2
  exit 1
fi
if [[ "$current_branch" != "$expected_branch" ]]; then
  echo "branch changed while confirming push: expected $expected_branch, found $current_branch" >&2
  exit 1
fi
current_oid=$(git rev-parse --verify 'HEAD^{commit}')
if [[ "$current_oid" != "$expected_oid" ]]; then
  echo "commit changed while confirming push: expected $expected_oid, found $current_oid" >&2
  exit 1
fi

object_directory=$(git rev-parse --path-format=absolute --git-path objects)
push_directory=$(mktemp -d)
trap 'rm -rf "$push_directory"' EXIT
git init --bare --template= "$push_directory" >/dev/null

export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_COUNT=0
export GIT_ALTERNATE_OBJECT_DIRECTORIES="$object_directory"
export GIT_SSH_COMMAND='ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=/root/.ssh/known_hosts -o GlobalKnownHostsFile=/dev/null -o ConnectTimeout=10 -o ServerAliveInterval=10 -o ServerAliveCountMax=3'
git --git-dir="$push_directory" push --no-verify "$repository" "$expected_oid:refs/heads/$expected_branch"
`
	const result = await runProcess(
		[
			'ssh',
			'-A',
			...trustedGuestSSHOptions,
			'-o', 'ControlMaster=no',
			'-o', 'ControlPath=none',
			'-o', 'ServerAliveInterval=10',
			'-o', 'ServerAliveCountMax=3',
			`root@${managed.instance}`,
			`bash -s -- ${base64(managed.repository)} ${base64(branch)} ${base64(oid)}`,
		],
		{ stdin: script, allowFailure: true, timeoutMs: pushTimeoutMs },
	)
	if (result.exitCode !== 0) {
		await ctx.ui.notify(`✗ srv Runner: push failed for ${managed.instance}:\n${failureMessage(result)}`)
		return
	}

	const output = [result.stdout.trim(), result.stderr.trim()].filter(Boolean).join('\n') || 'Everything up-to-date'
	await ctx.ui.notify(`✓ srv Runner: pushed ${branch} from ${managed.instance}.\n${output.slice(-2_000)}`)
}

async function managedThreadForContext(ctx: PluginCommandContext): Promise<ManagedInstance | undefined> {
	if (!ctx.thread) {
		await ctx.ui.notify('srv Runner: no active Amp thread.')
		return
	}
	const managed = (await readInstances()).find((candidate) => candidate.threadID === ctx.thread?.id)
	if (!managed) {
		await ctx.ui.notify('srv Runner: the active thread is not associated with a managed srv VM.')
		return
	}
	return managed
}

async function readInstances(): Promise<ManagedInstance[]> {
	let names: string[]
	try {
		names = await readdir(stateDirectory)
	} catch (error) {
		if ((error as NodeJS.ErrnoException).code === 'ENOENT') return []
		throw error
	}
	const instances = await Promise.all(
		names
			.filter((name) => name.endsWith('.json'))
			.map(async (name) => JSON.parse(await readFile(join(stateDirectory, name), 'utf8')) as ManagedInstance),
	)
	return instances.sort((left, right) => right.createdAt.localeCompare(left.createdAt))
}

async function writeInstance(managed: ManagedInstance) {
	await mkdir(stateDirectory, { recursive: true, mode: 0o700 })
	const path = join(stateDirectory, `${managed.instance}.json`)
	const temporaryPath = `${path}.${process.pid}.${crypto.randomUUID()}.tmp`
	await writeFile(temporaryPath, `${JSON.stringify(managed, null, 2)}\n`, { mode: 0o600 })
	await chmod(temporaryPath, 0o600)
	await rename(temporaryPath, path)
}

async function deleteInstanceRecord(instance: string) {
	try {
		await unlink(join(stateDirectory, `${instance}.json`))
	} catch (error) {
		if ((error as NodeJS.ErrnoException).code !== 'ENOENT') throw error
	}
}

async function runProcess(args: string[], options: ProcessOptions = {}): Promise<ProcessResult> {
	const process = Bun.spawn(args, {
		cwd: options.cwd,
		env: globalThis.process.env,
		stdin: options.stdin === undefined ? 'ignore' : 'pipe',
		stdout: 'pipe',
		stderr: 'pipe',
	})

	if (options.stdin !== undefined) {
		process.stdin.write(options.stdin)
		process.stdin.end()
	}
	let timedOut = false
	const timeout = options.timeoutMs
		? setTimeout(() => {
			timedOut = true
			process.kill()
		}, options.timeoutMs)
		: undefined

	let stdout: string
	let stderr: string
	let exitCode: number
	try {
		;[stdout, stderr, exitCode] = await Promise.all([
			new Response(process.stdout).text(),
			new Response(process.stderr).text(),
			process.exited,
		])
	} finally {
		if (timeout) clearTimeout(timeout)
	}
	const result = { stdout, stderr, exitCode, timedOut }
	if (exitCode !== 0 && !options.allowFailure) {
		throw new Error(`${args[0]} exited with status ${exitCode}: ${failureMessage(result)}`)
	}
	return result
}

async function runBinaryProcess(args: string[]): Promise<BinaryProcessResult> {
	const process = Bun.spawn(args, {
		env: globalThis.process.env,
		stdin: 'ignore',
		stdout: 'pipe',
		stderr: 'pipe',
	})
	const [stdout, stderr, exitCode] = await Promise.all([
		new Response(process.stdout).arrayBuffer(),
		new Response(process.stderr).text(),
		process.exited,
	])
	const result = { stdout: new Uint8Array(stdout), stderr, exitCode }
	if (exitCode !== 0) {
		throw new Error(`${args[0]} exited with status ${exitCode}: ${stderr.trim() || 'unknown error'}`)
	}
	return result
}

function createProgressDisplay(amp: PluginAPI, instance: string, totalSteps: number): ProgressDisplay {
	const item: StatusItem = amp.createStatusItem()
	const shortName = instance.length > 28 ? `${instance.slice(0, 25)}…` : instance
	let disposed = false

	return {
		update(step, label) {
			if (disposed) return
			const completed = Math.max(0, Math.min(totalSteps, step))
			const bar = `${'●'.repeat(completed)}${'○'.repeat(totalSteps - completed)}`
			item.update({
				text: `${bar} srv ${completed}/${totalSteps} · ${label} · ${shortName}`,
				url: 'command:srv-runner.list-managed-vms',
			})
		},
		dispose() {
			if (disposed) return
			disposed = true
			item.unsubscribe()
		},
	}
}

function elapsedSeconds(startedAt: number): number {
	return Math.max(0, Math.round((Date.now() - startedAt) / 1_000))
}

function resourcesForSize(size: string) {
	switch (size) {
		case '2 CPU / 4 GiB / 20 GiB':
			return { cpus: 2, ramGiB: 4, diskGiB: 20 }
		case '8 CPU / 16 GiB / 40 GiB':
			return { cpus: 8, ramGiB: 16, diskGiB: 40 }
		default:
			return { cpus: 4, ramGiB: 8, diskGiB: 30 }
	}
}

function instanceName(repository: string): string {
	const repositoryName = basename(repository.replace(/\.git$/, ''))
	const slug = repositoryName
		.toLowerCase()
		.replace(/[^a-z0-9-]+/g, '-')
		.replace(/^-+|-+$/g, '')
		.slice(0, 36)
	const suffix = crypto.randomUUID().replaceAll('-', '').slice(0, 10)
	return `amp-${slug || 'repo'}-${suffix}`
}

function base64(value: string): string {
	return Buffer.from(value, 'utf8').toString('base64')
}

function isSSHRepository(repository: string): boolean {
	if (/[\x00-\x20\x7f]/.test(repository)) return false
	if (repository.startsWith('ssh://')) {
		try {
			const parsed = new URL(repository)
			return parsed.protocol === 'ssh:' && Boolean(parsed.hostname) && Boolean(parsed.pathname) && !parsed.password
		} catch {
			return false
		}
	}
	return /^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s]+$/.test(repository)
}

function parseInstanceSummary(output: string): string[] {
	try {
		const parsed = JSON.parse(output) as {
			instance?: {
				state?: string
				tailscale_name?: string
				tailscale_ip?: string
				vcpu_count?: number
				memory_mib?: number
			}
			state?: string
			tailscale_name?: string
			tailscale_ip?: string
			vcpu_count?: number
			memory_mib?: number
		}
		const instance = parsed.instance ?? parsed
		return [
			`State: ${instance.state ?? 'unknown'}`,
			`Tailscale: ${instance.tailscale_name ?? instance.tailscale_ip ?? 'unknown'}`,
			`Resources: ${instance.vcpu_count ?? '?'} CPU / ${instance.memory_mib ?? '?'} MiB`,
		]
	} catch {
		return ['State: inspect returned invalid JSON']
	}
}

function failureMessage(result: ProcessResult): string {
	if (result.timedOut) return `timed out after ${pushTimeoutMs / 1_000} seconds`
	return (result.stderr.trim() || result.stdout.trim() || 'unknown error').slice(-2_000)
}

function errorMessage(error: unknown): string {
	return error instanceof Error ? error.message : String(error)
}

function sleep(milliseconds: number) {
	return new Promise((resolve) => setTimeout(resolve, milliseconds))
}
