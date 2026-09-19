import { useEffect, useState, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { apiClient } from "../../lib/api-client";
import { aoBridge } from "../../lib/bridge";
import { useCloudSession } from "../../lib/cloud-session";
import { useCloudLocalAuth } from "../../hooks/useCloudLocalAuth";
import { useLocalSignInDialogStore } from "../../stores/local-signin-dialog-store";
import { useSettings } from "../../hooks/useSettings";
import { createRendererCloudCpClient } from "../../hooks/useCloudCp";
import {
	activateLinearRunner,
	disconnectLinearRunner,
	linearRequest,
	prepareLinearRunner,
	useIntegrationOrganizations,
	useLinearRunners,
	useLinearSettings,
	type LinearConnection,
	type LinearProfile,
	type LinearResource,
} from "../../hooks/useLinearIntegration";
import { Button } from "../ui/button";
import { SettingsSection } from "./SettingsSection";

const inputClass =
	"w-full rounded-md border border-[var(--color-border-settings-input)] bg-[var(--color-bg-settings-input)] px-3 py-2 text-sm";
function Field({ label, children }: { label: string; children: ReactNode }) {
	return (
		<label className="flex flex-col gap-1.5 text-xs text-settings-muted">
			{label}
			{children}
		</label>
	);
}
// Linear brand mark from Simple Icons (CC0): https://simpleicons.org/?q=linear
export function LinearLogo() {
	return (
		<svg
			aria-hidden="true"
			viewBox="0 0 24 24"
			className="size-6 shrink-0"
			fill="currentColor"
		>
			<path d="M2.886 4.18A11.982 11.982 0 0 1 11.99 0C18.624 0 24 5.376 24 12.009c0 3.64-1.62 6.903-4.18 9.105L2.887 4.18ZM1.817 5.626l16.556 16.556c-.524.33-1.075.62-1.65.866L.951 7.277c.247-.575.537-1.126.866-1.65ZM.322 9.163l14.515 14.515c-.71.172-1.443.282-2.195.322L0 11.358a12 12 0 0 1 .322-2.195Zm-.17 4.862 9.823 9.824a12.02 12.02 0 0 1-9.824-9.824Z" />
		</svg>
	);
}

export function LinearIntegrationSection({
	titleHidden,
}: {
	titleHidden?: boolean;
}) {
	const { settings } = useSettings();
	const { t } = useTranslation();
	const baseUrl = settings?.cloudControlPlaneUrl ?? "";
	if (!baseUrl)
		return (
			<SettingsSection
				title={t("settings.integrations")}
				titleHidden={titleHidden}
			>
				<div className="flex flex-col gap-3 rounded-md bg-[var(--color-bg-settings-row)] p-4">
					<div className="flex items-center gap-3">
						<LinearLogo />
						<h3 className="text-sm font-medium">{t("linear.title")}</h3>
					</div>
					<p className="text-xs text-settings-muted">
						{t("linear.unavailable")}
					</p>
				</div>
			</SettingsSection>
		);
	return (
		<LinearIntegrationContent
			key={baseUrl}
			baseUrl={baseUrl}
			titleHidden={titleHidden}
		/>
	);
}
function LinearIntegrationContent({
	titleHidden,
	baseUrl,
}: {
	titleHidden?: boolean;
	baseUrl: string;
}) {
	const { t } = useTranslation();
	const auth = useCloudSession();
	const { available: localAuthAvailable } = useCloudLocalAuth();
	const openLocalSignIn = useLocalSignInDialogStore((s) => s.openDialog);
	const accountId = auth.session?.user.id ?? "";
	const signedIn = auth.status === "authenticated";
	const organizations = useIntegrationOrganizations(
		baseUrl,
		signedIn,
		accountId,
	);
	const [selectedOrg, setSelectedOrg] = useState("");
	const orgId = organizations.data?.organizations.some(
		(o) => o.id === selectedOrg,
	)
		? selectedOrg
		: (organizations.data?.organizations[0]?.id ?? "");
	const query = useLinearSettings(baseUrl, orgId, signedIn, accountId);
	const runners = useLinearRunners();
	const cache = useQueryClient();
	const [editing, setEditing] = useState<LinearProfile | "new" | null>(null);
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [authorizing, setAuthorizing] = useState(false);
	useEffect(() => {
		setEditing(null);
		setAuthorizing(false);
		setError("");
	}, [orgId, accountId]);
	useEffect(() => {
		if (!authorizing) return;
		const timer = setTimeout(() => {
			setAuthorizing(false);
			setError(t("linear.authorizationExpired"));
		}, 600_000);
		return () => clearTimeout(timer);
	}, [authorizing, t]);
	const refresh = async () => {
		await Promise.all([
			cache.invalidateQueries({ queryKey: ["linear-integrations"] }),
			cache.invalidateQueries({ queryKey: ["linear-runners"] }),
		]);
	};
	const run = async (fn: () => Promise<void>) => {
		setBusy(true);
		setError("");
		try {
			await fn();
			await refresh();
		} catch (e) {
			setError(e instanceof Error ? e.message : t("linear.failed"));
		} finally {
			setBusy(false);
		}
	};
	const connect = () =>
		run(async () => {
			const result = await linearRequest<{ authorizationUrl: string }>(
				baseUrl,
				orgId,
				"/connect",
				"POST",
			);
			await aoBridge.app.openExternal(result.authorizationUrl);
			setAuthorizing(true);
		});
	const connections = signedIn ? (query.data?.connections ?? []) : [];
	const profiles = signedIn ? (query.data?.profiles ?? []) : [];
	return (
		<SettingsSection
			title={t("settings.integrations")}
			titleHidden={titleHidden}
		>
			<div className="flex flex-col gap-5 rounded-md bg-[var(--color-bg-settings-row)] p-4">
				<div className="flex items-start gap-3">
					<LinearLogo />
					<div className="flex-1">
						<h3 className="text-sm font-medium">{t("linear.title")}</h3>
						<p className="mt-1 text-xs leading-relaxed text-settings-muted">
							{t("linear.description")}
						</p>
					</div>
				</div>
				{!baseUrl ? (
					<p className="text-sm text-settings-muted">
						{t("linear.unavailable")}
					</p>
				) : !signedIn ? (
					<div className="flex items-center justify-between gap-4">
						<p className="text-xs text-settings-muted">
							{t("linear.signInDescription")}
						</p>
						<Button
							variant="footer"
							onClick={() =>
								localAuthAvailable ? openLocalSignIn() : auth.signIn()
							}
							disabled={auth.status === "loading"}
						>
							{t("linear.signIn")}
						</Button>
					</div>
				) : (
					<>
						{organizations.data &&
						organizations.data.organizations.length === 0 ? (
							<Button
								variant="footer"
								disabled={busy}
								onClick={() =>
									void run(async () => {
										await createRendererCloudCpClient(
											baseUrl,
										).createOrganization({ displayName: "My workspace" });
										await organizations.refetch();
									})
								}
							>
								{t("linear.createOrganization")}
							</Button>
						) : null}
						{organizations.data &&
						organizations.data.organizations.length > 1 ? (
							<Field label={t("linear.organization")}>
								<select
									className={inputClass}
									value={orgId}
									onChange={(e) => setSelectedOrg(e.target.value)}
								>
									{organizations.data.organizations.map((o) => (
										<option value={o.id} key={o.id}>
											{o.displayName}
										</option>
									))}
								</select>
							</Field>
						) : null}
						{(organizations.isLoading || query.isLoading) && (
							<p role="status" className="text-sm text-settings-muted">
								{t("linear.loading")}
							</p>
						)}
						{query.data?.available === false && (
							<p className="text-sm text-settings-muted">
								{t("linear.unavailable")}
							</p>
						)}
						{query.data?.available && (
							<>
								{connections.map((c) => (
									<div
										key={c.id}
										className="flex flex-col gap-2 border-t border-border pt-4"
									>
										<div className="flex items-center justify-between gap-3">
											<div>
												<p className="text-sm">{c.workspace.name}</p>
												<p className="mt-1 text-xs text-settings-muted">
													{t("linear.installedAgent", {
														name: c.agent.name,
														handle: c.agent.displayName,
													})}
												</p>
											</div>
											<Button
												variant="footer"
												disabled={busy}
												onClick={() => void connect()}
											>
												{t("linear.reconnect")}
											</Button>
										</div>
										{c.error && (
											<p role="alert" className="text-xs text-destructive">
												{t("linear.reconnectNeeded")}
											</p>
										)}
										{!profiles.some((p) => p.connectionId === c.id) && (
											<p className="text-xs text-settings-muted">
												{t("linear.incomplete")}
											</p>
										)}
									</div>
								))}
								{profiles.map((p) => {
									const local = runners.data?.find((r) => r.id === p.runnerId);
									const online = Date.now() - Date.parse(p.lastSeen) < 30_000;
									return (
										<div
											key={p.id}
											className="flex flex-col gap-2 rounded-md border border-border p-3"
										>
											<div className="flex justify-between gap-3">
												<span className="text-sm font-medium">{p.name}</span>
												<span className="text-xs text-settings-muted">
													{p.paused
														? t("linear.paused")
														: online
															? t("linear.ready")
															: t("linear.waiting")}
												</span>
											</div>
											<p className="text-xs text-settings-muted">
												{
													connections
														.find((c) => c.id === p.connectionId)
														?.teams.find((v) => v.id === p.teamId)?.name
												}{" "}
												· {p.localProjectId}
											</p>
											{local?.error && (
												<p role="status" className="text-xs text-destructive">
													{t("linear.runnerError")}
												</p>
											)}
											<div className="flex flex-wrap gap-2">
												<Button
													variant="footer"
													disabled={busy}
													onClick={() => setEditing(p)}
												>
													{t("linear.edit")}
												</Button>
												<Button
													variant="footer"
													disabled={busy}
													onClick={() =>
														void run(async () => {
															await linearRequest(
																baseUrl,
																orgId,
																"/profiles",
																"PUT",
																{ ...p, paused: !p.paused },
															);
														})
													}
												>
													{p.paused ? t("linear.resume") : t("linear.pause")}
												</Button>
												{local && !local.active && (
													<Button
														variant="footer"
														disabled={busy}
														onClick={() =>
															void run(() =>
																activateLinearRunner(local.id, p.id),
															)
														}
													>
														{t("linear.finishSetup")}
													</Button>
												)}
												<Button
													variant="footer"
													disabled={busy}
													onClick={() =>
														void run(async () => {
															await linearRequest(
																baseUrl,
																orgId,
																`/profiles/${encodeURIComponent(p.id)}`,
																"DELETE",
															);
															if (local) await disconnectLinearRunner(local.id);
														})
													}
												>
													{t("linear.disconnect")}
												</Button>
											</div>
										</div>
									);
								})}
								{editing ? (
									<ProfileForm
										key={typeof editing === "string" ? "new" : editing.id}
										baseUrl={baseUrl}
										orgId={orgId}
										connections={connections}
										profile={editing === "new" ? undefined : editing}
										onCancel={() => setEditing(null)}
										onSaved={async () => {
											setEditing(null);
											await refresh();
										}}
									/>
								) : (
									<div className="flex flex-wrap gap-2">
										<Button
											variant="footer-primary"
											disabled={busy || !orgId}
											onClick={() => void connect()}
										>
											{connections.length
												? t("linear.connectAnother")
												: t("linear.connect")}
										</Button>
										{connections.length > 0 && (
											<Button
												variant="footer"
												onClick={() => {
													setAuthorizing(false);
													setEditing("new");
												}}
											>
												{t("linear.addProfile")}
											</Button>
										)}
									</div>
								)}
								{authorizing && (
									<div
										role="status"
										className="flex items-center justify-between gap-3 text-xs text-settings-muted"
									>
										<p>{t("linear.waitingAuthorization")}</p>
										<Button
											variant="ghost"
											size="sm"
											onClick={() => {
												setAuthorizing(false);
												void refresh();
											}}
										>
											{t("linear.done")}
										</Button>
									</div>
								)}
							</>
						)}
					</>
				)}
				{(error || organizations.error || query.error || runners.error) && (
					<p role="alert" className="text-xs text-destructive">
						{error ||
							(organizations.error as Error)?.message ||
							(query.error as Error)?.message ||
							(runners.error as Error)?.message}
					</p>
				)}
			</div>
		</SettingsSection>
	);
}
function ProfileForm({
	baseUrl,
	orgId,
	connections,
	profile,
	onCancel,
	onSaved,
}: {
	baseUrl: string;
	orgId: string;
	connections: LinearConnection[];
	profile?: LinearProfile;
	onCancel: () => void;
	onSaved: () => Promise<void>;
}) {
	const { t } = useTranslation();
	const [connectionId, setConnectionId] = useState(
		profile?.connectionId ?? connections[0]?.id ?? "",
	);
	const connection = connections.find((c) => c.id === connectionId);
	const [teamId, setTeamId] = useState(
		profile?.teamId ?? connection?.teams[0]?.id ?? "",
	);
	const [linearProjectId, setLinearProjectId] = useState(
		profile?.linearProjectId ?? "",
	);
	const [name, setName] = useState(profile?.name ?? "Lenticular");
	const [projectId, setProjectId] = useState(profile?.localProjectId ?? "");
	const [busy, setBusy] = useState(false);
	const [error, setError] = useState("");
	const [savedProfile, setSavedProfile] = useState<LinearProfile>();
	const projects = useQuery({
		queryKey: ["linear-local-projects"],
		queryFn: async () => {
			const { data, error } = await apiClient.GET("/api/v1/projects");
			if (error) throw new Error(t("linear.projectsFailed"));
			return data?.projects ?? [];
		},
	});
	const linearProjects = useQuery({
		queryKey: ["linear-projects", baseUrl, orgId, connectionId, teamId],
		enabled: !!connectionId && !!teamId,
		queryFn: () =>
			linearRequest<{ projects: LinearResource[] }>(
				baseUrl,
				orgId,
				`/projects?connectionId=${encodeURIComponent(connectionId)}&teamId=${encodeURIComponent(teamId)}`,
			),
	});
	const save = async () => {
		setBusy(true);
		setError("");
		try {
			let saved = savedProfile;
			if (!saved) {
				const runner = profile
					? undefined
					: await prepareLinearRunner(projectId);
				saved = await linearRequest<LinearProfile>(
					baseUrl,
					orgId,
					"/profiles",
					"PUT",
					{
						...profile,
						id: profile?.id ?? "",
						connectionId,
						name,
						teamId,
						linearProjectId,
						localProjectId: projectId,
						runnerId: profile?.runnerId ?? runner!.id,
						challenge: runner?.challenge,
						paused: profile?.paused ?? false,
					},
				);
				setSavedProfile(saved);
			}
			if (!profile) await activateLinearRunner(saved.runnerId, saved.id);
			await onSaved();
		} catch (e) {
			setError(e instanceof Error ? e.message : t("linear.failed"));
		} finally {
			setBusy(false);
		}
	};
	return (
		<form
			className="flex flex-col gap-4 border-t border-border pt-4"
			onSubmit={(e) => {
				e.preventDefault();
				void save();
			}}
		>
			<fieldset
				disabled={busy || !!savedProfile}
				className="flex flex-col gap-4"
			>
				<Field label={t("linear.workspace")}>
					<select
						className={inputClass}
						value={connectionId}
						disabled={!!profile}
						onChange={(e) => {
							setConnectionId(e.target.value);
							setTeamId(
								connections.find((c) => c.id === e.target.value)?.teams[0]
									?.id ?? "",
							);
							setLinearProjectId("");
						}}
					>
						{connections.map((c) => (
							<option key={c.id} value={c.id}>
								{c.workspace.name}
							</option>
						))}
					</select>
				</Field>
				<div className="grid grid-cols-2 gap-3">
					<Field label={t("linear.team")}>
						<select
							className={inputClass}
							value={teamId}
							onChange={(e) => {
								setTeamId(e.target.value);
								setLinearProjectId("");
							}}
						>
							{connection?.teams.map((v) => (
								<option key={v.id} value={v.id}>
									{v.name}
								</option>
							))}
						</select>
					</Field>
					<Field label={t("linear.projectFilter")}>
						<select
							className={inputClass}
							value={linearProjectId}
							disabled={linearProjects.isLoading}
							onChange={(e) => setLinearProjectId(e.target.value)}
						>
							<option value="">{t("linear.allProjects")}</option>
							{linearProjects.data?.projects.map((v) => (
								<option key={v.id} value={v.id}>
									{v.name}
								</option>
							))}
						</select>
					</Field>
				</div>
				<Field label={t("linear.profileName")}>
					<input
						className={inputClass}
						value={name}
						required
						maxLength={64}
						onChange={(e) => setName(e.target.value)}
					/>
				</Field>
				<p className="-mt-2 text-xs text-settings-muted">
					{t("linear.nameHelp", { name: connection?.agent.name })}
				</p>
				<Field label={t("linear.localProject")}>
					<select
						className={inputClass}
						value={projectId}
						required
						disabled={!!profile}
						onChange={(e) => setProjectId(e.target.value)}
					>
						<option value="">{t("linear.chooseProject")}</option>
						{projects.data?.map((p) => (
							<option key={p.id} value={p.id}>
								{p.name || p.id}
							</option>
						))}
					</select>
				</Field>
				<p className="text-xs text-settings-muted">{t("linear.localRunner")}</p>
			</fieldset>
			{(error || linearProjects.error || projects.error) && (
				<p role="alert" className="text-xs text-destructive">
					{error ||
						(linearProjects.error as Error)?.message ||
						(projects.error as Error)?.message}
				</p>
			)}
			<div className="flex justify-end gap-2">
				<Button
					type="button"
					variant="footer"
					disabled={busy}
					onClick={onCancel}
				>
					{t("linear.cancel")}
				</Button>
				<Button
					type="submit"
					variant="footer-primary"
					disabled={
						busy ||
						!name.trim() ||
						!teamId ||
						!projectId ||
						linearProjects.isLoading ||
						!!linearProjects.error
					}
				>
					{busy
						? t("linear.saving")
						: savedProfile
							? t("linear.finishSetup")
							: profile
								? t("linear.save")
								: t("linear.enable")}
				</Button>
			</div>
		</form>
	);
}
