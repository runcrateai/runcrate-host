// get.runcrate.ai: serves the host installer and agent releases, read-only,
// from the R2 bucket runcrate-host-images (hack/publish.sh uploads them).
// Only these keys are served; everything else in the bucket stays out:
//
//   host, host.sig, host.version
//   releases/<version or latest>/runcrate-host-linux-amd64{,.sig,.version}
const SERVED = /^(host(\.sig|\.version)?|releases\/[A-Za-z0-9._+-]+\/runcrate-host-linux-amd64(\.sig|\.version)?)$/;

export default {
	async fetch(request, env) {
		if (request.method !== "GET" && request.method !== "HEAD") {
			return new Response("method not allowed\n", { status: 405, headers: { allow: "GET, HEAD" } });
		}
		const key = new URL(request.url).pathname.slice(1);
		const object = SERVED.test(key)
			? await (request.method === "HEAD" ? env.RELEASES.head(key) : env.RELEASES.get(key))
			: null;
		if (!object) {
			return new Response("not found\n", { status: 404, headers: { "content-type": "text/plain; charset=utf-8" } });
		}
		const headers = new Headers({ etag: object.httpEtag, "x-content-type-options": "nosniff" });
		object.writeHttpMetadata(headers);
		if (request.method === "HEAD") {
			headers.set("content-length", String(object.size));
			return new Response(null, { headers });
		}
		return new Response(object.body, { headers });
	},
};
