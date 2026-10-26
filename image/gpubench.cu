// rc-gpubench: proves a GPU is what it says it is, or wipes its memory.
//
//   rc-gpubench          memory test (two patterns over nearly all VRAM) and an
//                        FP16 matmul (FP32 accumulate) checked against the CPU
//   rc-gpubench --scrub  overwrite nearly all VRAM with zeros and verify
//
// Prints one JSON object on stdout. Built into the Runcrate VM image and run
// only inside throwaway VMs by the host agent.
#include <cublas_v2.h>
#include <cuda_fp16.h>
#include <cuda_runtime.h>

#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <random>
#include <string>
#include <vector>

#define CK(x)                                                     \
  do {                                                            \
    cudaError_t e_ = (x);                                         \
    if (e_ != cudaSuccess) {                                      \
      err = std::string(#x) + ": " + cudaGetErrorString(e_);      \
      return false;                                               \
    }                                                             \
  } while (0)

__global__ void fill(uint64_t* p, size_t n, uint64_t seed) {
  for (size_t i = blockIdx.x * (size_t)blockDim.x + threadIdx.x; i < n; i += (size_t)gridDim.x * blockDim.x)
    p[i] = seed ^ (i * 0x9E3779B97F4A7C15ull);
}

__global__ void check(const uint64_t* p, size_t n, uint64_t seed, bool zero, unsigned long long* bad) {
  for (size_t i = blockIdx.x * (size_t)blockDim.x + threadIdx.x; i < n; i += (size_t)gridDim.x * blockDim.x) {
    uint64_t want = zero ? 0ull : (seed ^ (i * 0x9E3779B97F4A7C15ull));
    if (p[i] != want) atomicAdd(bad, 1ull);
  }
}

static std::string esc(const std::string& s) {
  std::string o;
  for (char c : s) {
    if (c == '"' || c == '\\') { o += '\\'; o += c; }
    else if ((unsigned char)c < 0x20) o += ' ';
    else o += c;
  }
  return o;
}

struct Result {
  int ordinal = 0;
  std::string name, uuid, pci;
  double vram_gb = 0, mem_tested_gb = 0, tflops = 0;
  bool mem_ok = false, matmul_ok = false;
  std::string err;
};

struct Chunk {
  uint64_t* p;
  size_t bytes;
};

// Allocates nearly all free memory: big chunks first, then smaller ones, so
// only a few MB the driver keeps for itself are left untouched.
static std::vector<Chunk> grab(size_t* total) {
  std::vector<Chunk> v;
  *total = 0;
  const size_t sizes[] = {512ull << 20, 64ull << 20, 8ull << 20, 2ull << 20};
  for (size_t sz : sizes) {
    for (;;) {
      uint64_t* p = nullptr;
      if (cudaMalloc(&p, sz) != cudaSuccess) { cudaGetLastError(); break; }
      v.push_back({p, sz});
      *total += sz;
    }
  }
  return v;
}

static void release(std::vector<Chunk>& v) {
  for (auto& c : v) cudaFree(c.p);
  v.clear();
}

static bool memtest(Result& r, bool scrub, std::string& err) {
  size_t total = 0;
  unsigned long long* bad = nullptr;
  CK(cudaMalloc(&bad, sizeof(*bad)));
  CK(cudaMemset(bad, 0, sizeof(*bad)));
  auto chunks = grab(&total);
  if (chunks.empty()) { err = "could not allocate GPU memory"; return false; }
  if (scrub) {
    for (auto& c : chunks) CK(cudaMemset(c.p, 0, c.bytes));
    CK(cudaDeviceSynchronize());
    for (auto& c : chunks) check<<<1024, 256>>>(c.p, c.bytes / 8, 0, true, bad);
  } else {
    const uint64_t seeds[2] = {0x5555555555555555ull, 0xAAAAAAAAAAAAAAAAull};
    for (uint64_t seed : seeds) {
      for (size_t i = 0; i < chunks.size(); i++) fill<<<1024, 256>>>(chunks[i].p, chunks[i].bytes / 8, seed + i);
      CK(cudaDeviceSynchronize());
      for (size_t i = 0; i < chunks.size(); i++) check<<<1024, 256>>>(chunks[i].p, chunks[i].bytes / 8, seed + i, false, bad);
    }
    // Leave it zeroed.
    for (auto& c : chunks) CK(cudaMemset(c.p, 0, c.bytes));
  }
  CK(cudaDeviceSynchronize());
  unsigned long long h = 0;
  CK(cudaMemcpy(&h, bad, sizeof(h), cudaMemcpyDeviceToHost));
  release(chunks);
  cudaFree(bad);
  r.mem_tested_gb = total / 1073741824.0;
  r.mem_ok = h == 0;
  if (h) err = std::to_string(h) + " words read back wrong";
  return true;
}

static bool matmul(Result& r, std::string& err) {
  const int N = 8192;
  std::mt19937 rng(42);
  std::uniform_int_distribution<int> d(-2, 2);
  std::vector<__half> ha((size_t)N * N), hb((size_t)N * N);
  std::vector<float> fa((size_t)N * N), fb((size_t)N * N);
  for (size_t i = 0; i < ha.size(); i++) {
    fa[i] = (float)d(rng); fb[i] = (float)d(rng);
    ha[i] = __float2half(fa[i]); hb[i] = __float2half(fb[i]);
  }
  __half *a, *b;
  float* c;
  CK(cudaMalloc(&a, ha.size() * sizeof(__half)));
  CK(cudaMalloc(&b, hb.size() * sizeof(__half)));
  CK(cudaMalloc(&c, (size_t)N * N * sizeof(float)));
  CK(cudaMemcpy(a, ha.data(), ha.size() * sizeof(__half), cudaMemcpyHostToDevice));
  CK(cudaMemcpy(b, hb.data(), hb.size() * sizeof(__half), cudaMemcpyHostToDevice));
  cublasHandle_t hdl;
  if (cublasCreate(&hdl) != CUBLAS_STATUS_SUCCESS) { err = "cublasCreate failed"; return false; }
  float alpha = 1, beta = 0;
  auto run = [&]() {
    return cublasGemmEx(hdl, CUBLAS_OP_N, CUBLAS_OP_N, N, N, N, &alpha, a, CUDA_R_16F, N, b, CUDA_R_16F, N, &beta, c,
                        CUDA_R_32F, N, CUBLAS_COMPUTE_32F, CUBLAS_GEMM_DEFAULT_TENSOR_OP);
  };
  for (int i = 0; i < 3; i++)
    if (run() != CUBLAS_STATUS_SUCCESS) { err = "cublasGemmEx failed"; return false; }
  CK(cudaDeviceSynchronize());
  const int iters = 20;
  cudaEvent_t s, e;
  cudaEventCreate(&s); cudaEventCreate(&e);
  cudaEventRecord(s);
  for (int i = 0; i < iters; i++) run();
  cudaEventRecord(e);
  CK(cudaEventSynchronize(e));
  float ms = 0;
  cudaEventElapsedTime(&ms, s, e);
  r.tflops = 2.0 * N * (double)N * N * iters / (ms / 1000.0) / 1e12;
  // Column-major: C[i + j*N] = sum_k A[i + k*N] * B[k + j*N]. Exact in FP32.
  std::vector<float> hc((size_t)N * N);
  CK(cudaMemcpy(hc.data(), c, hc.size() * sizeof(float), cudaMemcpyDeviceToHost));
  int wrong = 0;
  for (int t = 0; t < 64; t++) {
    int i = rng() % N, j = rng() % N;
    double want = 0;
    for (int k = 0; k < N; k++) want += (double)fa[i + (size_t)k * N] * fb[k + (size_t)j * N];
    if (hc[i + (size_t)j * N] != (float)want) wrong++;
  }
  r.matmul_ok = wrong == 0;
  if (wrong) err = std::to_string(wrong) + " of 64 matmul results wrong";
  cublasDestroy(hdl);
  cudaFree(a); cudaFree(b); cudaFree(c);
  return true;
}

int main(int argc, char** argv) {
  bool scrub = argc > 1 && strcmp(argv[1], "--scrub") == 0;
  int n = 0;
  cudaError_t ce = cudaGetDeviceCount(&n);
  if (ce != cudaSuccess || n == 0) {
    printf("{\"ok\":false,\"error\":\"%s\",\"gpus\":[]}\n", esc(ce != cudaSuccess ? cudaGetErrorString(ce) : "no GPUs visible").c_str());
    return 1;
  }
  bool all = true;
  std::string out = "{\"gpus\":[";
  for (int i = 0; i < n; i++) {
    Result r;
    r.ordinal = i;
    std::string err;
    cudaSetDevice(i);
    cudaDeviceProp p;
    if (cudaGetDeviceProperties(&p, i) == cudaSuccess) {
      r.name = p.name;
      r.vram_gb = p.totalGlobalMem / 1073741824.0;
      char u[64];
      const unsigned char* b = (const unsigned char*)p.uuid.bytes;
      snprintf(u, sizeof u, "GPU-%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x", b[0], b[1], b[2], b[3],
               b[4], b[5], b[6], b[7], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15]);
      r.uuid = u;
      char pci[32];
      snprintf(pci, sizeof pci, "%04x:%02x:%02x.0", p.pciDomainID, p.pciBusID, p.pciDeviceID);
      r.pci = pci;
    }
    bool ran = memtest(r, scrub, err);
    if (ran && !scrub && r.mem_ok) {
      std::string merr;
      if (!matmul(r, merr)) err = merr;
      else if (!merr.empty()) err = merr;
    }
    r.err = err;
    bool ok = ran && r.mem_ok && (scrub || r.matmul_ok);
    all = all && ok;
    char buf[1024];
    snprintf(buf, sizeof buf,
             "%s{\"ordinal\":%d,\"name\":\"%s\",\"uuid\":\"%s\",\"pci_bus_id\":\"%s\",\"vram_gb\":%.2f,\"mem_ok\":%s,"
             "\"mem_tested_gb\":%.2f,\"matmul_ok\":%s,\"matmul_tflops_fp16\":%.2f,\"ok\":%s,\"error\":\"%s\"}",
             i ? "," : "", i, esc(r.name).c_str(), r.uuid.c_str(), r.pci.c_str(), r.vram_gb, r.mem_ok ? "true" : "false",
             r.mem_tested_gb, r.matmul_ok ? "true" : "false", r.tflops, ok ? "true" : "false", esc(r.err).c_str());
    out += buf;
    cudaDeviceReset();
  }
  out += std::string("],\"ok\":") + (all ? "true" : "false") + ",\"mode\":\"" + (scrub ? "scrub" : "bench") + "\"}";
  printf("%s\n", out.c_str());
  return all ? 0 : 2;
}
