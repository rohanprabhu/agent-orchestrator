import { useQuery } from "@tanstack/react-query";
import { cloudCpFetch, createRendererCloudCpClient } from "./useCloudCp";
import { apiClient, apiErrorMessage } from "../lib/api-client";
import type { components } from "../../api/schema";

export interface LinearResource {
	id: string;
	name: string;
}
export interface LinearConnection {
	id: string;
	workspace: LinearResource;
	agent: { id: string; name: string; displayName: string };
	teams: LinearResource[];
	error?: string;
}
export interface LinearProfile {
	id: string;
	connectionId: string;
	name: string;
	teamId: string;
	linearProjectId: string;
	localProjectId: string;
	runnerId: string;
	paused: boolean;
	lastSeen: string;
	owner?: string;
	challenge?: string;
}
export interface LinearSettings {
	available: boolean;
	connections: LinearConnection[];
	profiles: LinearProfile[];
}
export type LinearRunner = components["schemas"]["LinearRunnerResponse"];

export async function linearRequest<T>(
	baseUrl: string,
	orgId: string,
	suffix = "",
	method = "GET",
	body?: unknown,
): Promise<T> {
	const response = await cloudCpFetch(
		`${baseUrl.replace(/\/$/, "")}/api/cloud/v1/orgs/${encodeURIComponent(orgId)}/linear${suffix}`,
		{
			method,
			headers: { "Content-Type": "application/json" },
			body: body === undefined ? undefined : JSON.stringify(body),
		},
	);
	if (!response.ok) {
		if (response.status === 401)
			throw new Error("Sign in to Lenticular to manage Linear.");
		if (response.status === 403)
			throw new Error(
				"A Lenticular organization administrator must manage this connection.",
			);
		if (response.status === 404 || response.status === 503)
			throw new Error(
				"Linear connections are not available on this service yet.",
			);
		if (response.status === 409)
			throw new Error(
				"That profile name or team/project scope is already in use. Choose another name or scope.",
			);
		throw new Error(
			"The Linear change could not be saved. Check your connection and workspace permissions, then retry.",
		);
	}
	return response.status === 204
		? (undefined as T)
		: (response.json() as Promise<T>);
}
export function useIntegrationOrganizations(
	baseUrl: string,
	signedIn: boolean,
	accountId: string,
) {
	return useQuery({
		queryKey: ["integration-organizations", baseUrl, accountId],
		enabled: signedIn && !!baseUrl,
		retry: 1,
		queryFn: () => createRendererCloudCpClient(baseUrl).me(),
	});
}
export function useLinearSettings(
	baseUrl: string,
	orgId: string,
	signedIn: boolean,
	accountId: string,
) {
	return useQuery({
		queryKey: ["linear-integrations", baseUrl, orgId, accountId],
		enabled: signedIn && !!baseUrl && !!orgId,
		queryFn: () => linearRequest<LinearSettings>(baseUrl, orgId),
		refetchInterval: 5_000,
		retry: 1,
	});
}
export function useLinearRunners() {
	return useQuery({
		queryKey: ["linear-runners"],
		refetchInterval: 5_000,
		queryFn: async () => {
			const { data, error } = await apiClient.GET(
				"/api/v1/settings/integrations/linear/runners",
			);
			if (error) throw new Error(apiErrorMessage(error));
			return data?.runners ?? [];
		},
	});
}
export async function prepareLinearRunner(projectId: string) {
	const { data, error } = await apiClient.POST(
		"/api/v1/settings/integrations/linear/runners",
		{ body: { projectId } },
	);
	if (error || !data)
		throw new Error(apiErrorMessage(error) || "Could not prepare this runner.");
	return data;
}
export async function activateLinearRunner(
	runnerId: string,
	profileId: string,
) {
	const { error } = await apiClient.POST(
		"/api/v1/settings/integrations/linear/runners/{runnerId}/activate",
		{ params: { path: { runnerId } }, body: { profileId } },
	);
	if (error) throw new Error(apiErrorMessage(error));
}
export async function disconnectLinearRunner(runnerId: string) {
	const { error } = await apiClient.DELETE(
		"/api/v1/settings/integrations/linear/runners/{runnerId}",
		{ params: { path: { runnerId } } },
	);
	if (error) throw new Error(apiErrorMessage(error));
}
