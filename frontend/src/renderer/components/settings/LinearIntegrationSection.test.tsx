import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, cleanup } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LinearIntegrationSection } from "./LinearIntegrationSection";

const mocks = vi.hoisted(() => ({
	signedIn: true,
	localAuthAvailable: false,
	localSignIn: vi.fn(),
	available: true,
	profiles: [] as Record<string, unknown>[],
	request: vi.fn(),
	prepare: vi.fn(),
	activate: vi.fn(),
	open: vi.fn(),
	signIn: vi.fn(),
}));
vi.mock("../../hooks/useCloudLocalAuth", () => ({
	useCloudLocalAuth: () => ({ available: mocks.localAuthAvailable }),
}));
vi.mock("../../stores/local-signin-dialog-store", () => ({
	useLocalSignInDialogStore: (
		select: (s: { openDialog: typeof mocks.localSignIn }) => unknown,
	) => select({ openDialog: mocks.localSignIn }),
}));
vi.mock("../../hooks/useSettings", () => ({
	useSettings: () => ({
		settings: { cloudControlPlaneUrl: "https://ao.test", cloudEnabled: false },
	}),
}));
vi.mock("../../lib/cloud-session", () => ({
	useCloudSession: () => ({
		status: mocks.signedIn ? "authenticated" : "unauthenticated",
		session: mocks.signedIn ? { user: { id: "user" } } : null,
		signIn: mocks.signIn,
	}),
}));
vi.mock("../../lib/bridge", () => ({
	aoBridge: { app: { openExternal: mocks.open } },
}));
vi.mock("../../lib/api-client", () => ({
	apiClient: {
		GET: vi.fn(async () => ({
			data: { projects: [{ id: "local", name: "Backend" }] },
		})),
	},
}));
vi.mock("../../hooks/useLinearIntegration", () => ({
	useIntegrationOrganizations: () => ({
		data: { organizations: [{ id: "org", displayName: "Acme" }] },
	}),
	useLinearSettings: () => ({
		data: {
			available: mocks.available,
			connections: mocks.available
				? [
						{
							id: "c",
							workspace: { id: "w", name: "Acme" },
							agent: { id: "a", name: "AO", displayName: "ao2" },
							teams: [{ id: "t", name: "Engineering" }],
						},
					]
				: [],
			profiles: mocks.profiles,
		},
	}),
	useLinearRunners: () => ({ data: [] }),
	linearRequest: mocks.request,
	prepareLinearRunner: mocks.prepare,
	activateLinearRunner: mocks.activate,
	disconnectLinearRunner: vi.fn(),
}));
function show() {
	const q = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	render(
		<QueryClientProvider client={q}>
			<LinearIntegrationSection />
		</QueryClientProvider>,
	);
	return q;
}
afterEach(cleanup);
beforeEach(() => {
	vi.clearAllMocks();
	mocks.signedIn = true;
	mocks.localAuthAvailable = false;
	mocks.available = true;
	mocks.profiles = [];
	mocks.request.mockImplementation(async (_base, _org, path) =>
		path.startsWith("/projects")
			? { projects: [] }
			: { authorizationUrl: "https://linear.app/oauth/authorize" },
	);
	mocks.prepare.mockResolvedValue({ id: "runner", challenge: "public-hash" });
	mocks.activate.mockResolvedValue(undefined);
});
describe("Linear settings", () => {
	it("uses local sign-in when local development auth is available", async () => {
		mocks.signedIn = false;
		mocks.localAuthAvailable = true;
		show();
		await userEvent.click(
			screen.getByRole("button", { name: "Sign in to Lenticular" }),
		);
		expect(mocks.localSignIn).toHaveBeenCalledOnce();
		expect(mocks.signIn).not.toHaveBeenCalled();
	});
	it("connects independently of cloud execution and shows the actual installed identity", async () => {
		show();
		expect(screen.getByText("Agent in Linear: AO · ao2")).toBeInTheDocument();
		await userEvent.click(
			screen.getByRole("button", { name: "Connect another workspace" }),
		);
		expect(mocks.request).toHaveBeenCalledWith(
			"https://ao.test",
			"org",
			"/connect",
			"POST",
		);
		expect(mocks.open).toHaveBeenCalledWith(
			"https://linear.app/oauth/authorize",
		);
	});
	it("shows unavailable and signed-out states without a fake connection", async () => {
		mocks.available = false;
		show();
		expect(
			screen.getByText(/not available on this service/),
		).toBeInTheDocument();
		expect(
			screen.queryByRole("button", { name: "Connect Linear" }),
		).not.toBeInTheDocument();
		cleanup();
		mocks.signedIn = false;
		show();
		await userEvent.click(
			screen.getByRole("button", { name: "Sign in to Lenticular" }),
		);
		expect(mocks.signIn).toHaveBeenCalled();
		expect(
			screen.queryByText("Agent in Linear: AO · ao2"),
		).not.toBeInTheDocument();
	});
	it("saves scope and public runner challenge, then activates locally", async () => {
		mocks.request.mockImplementation(
			async (_base, _org, path, _method, body) =>
				path.startsWith("/projects")
					? { projects: [] }
					: { ...body, id: "profile" },
		);
		show();
		await userEvent.click(
			screen.getByRole("button", { name: "Add agent profile" }),
		);
		await userEvent.type(
			screen.getByLabelText("Profile name in Lenticular"),
			" Backend",
		);
		await waitFor(() =>
			expect(
				screen.getByRole("option", { name: "Backend" }),
			).toBeInTheDocument(),
		);
		await userEvent.selectOptions(
			screen.getByLabelText("Lenticular project on this machine"),
			"local",
		);
		await userEvent.click(
			screen.getByRole("button", { name: "Enable agent profile" }),
		);
		await waitFor(() =>
			expect(mocks.activate).toHaveBeenCalledWith("runner", "profile"),
		);
		const call = mocks.request.mock.calls.find((c) => c[2] === "/profiles");
		expect(call?.[4]).toMatchObject({
			name: "Lenticular Backend",
			teamId: "t",
			localProjectId: "local",
			challenge: "public-hash",
		});
		expect(call?.[4]).not.toHaveProperty("token");
	});
	it("keeps the saved profile and retries activation after a transport failure", async () => {
		mocks.request.mockImplementation(
			async (_base, _org, path, _method, body) =>
				path.startsWith("/projects")
					? { projects: [] }
					: { ...body, id: "profile" },
		);
		mocks.activate.mockRejectedValueOnce(new Error("Offline"));
		show();
		await userEvent.click(
			screen.getByRole("button", { name: "Add agent profile" }),
		);
		await waitFor(() =>
			expect(
				screen.getByRole("option", { name: "Backend" }),
			).toBeInTheDocument(),
		);
		await userEvent.selectOptions(
			screen.getByLabelText("Lenticular project on this machine"),
			"local",
		);
		await userEvent.click(
			screen.getByRole("button", { name: "Enable agent profile" }),
		);
		await waitFor(() =>
			expect(screen.getByRole("alert")).toHaveTextContent("Offline"),
		);
		await userEvent.click(screen.getByRole("button", { name: "Finish setup" }));
		await waitFor(() => expect(mocks.activate).toHaveBeenCalledTimes(2));
		expect(
			mocks.request.mock.calls.filter((c) => c[2] === "/profiles"),
		).toHaveLength(1);
	});
});
