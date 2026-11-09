/* Synthetic management-only plugin: no network, account access or scheduler. */
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
typedef struct { void *ptr; size_t len; } buffer;
typedef struct { uint32_t abi_version; void *host_ctx; void *call; void *free_buffer; } host_api;
typedef struct { uint32_t abi_version; int (*call)(char*, uint8_t*, size_t, buffer*); void (*free_buffer)(void*, size_t); void (*shutdown)(void); } plugin_api;
static int call(char *method, uint8_t *request, size_t n, buffer *response) {
  (void)request; (void)n;
  const char *json = "{\"ok\":true,\"result\":{}}";
  if (!strcmp(method, "plugin.register") || !strcmp(method, "plugin.reconfigure"))
    json = "{\"ok\":true,\"result\":{\"schema_version\":6,\"metadata\":{\"Name\":\"Synthetic Keeper\",\"Version\":\"0.0.0-fixture\",\"Author\":\"Quota Guard tests\",\"GitHubRepository\":\"https://github.com/Wuse777/cpa-quota-guard\"},\"capabilities\":{\"management_api\":true,\"scheduler\":false}}}";
  response->len = strlen(json); response->ptr = malloc(response->len);
  memcpy(response->ptr, json, response->len); return 0;
}
static void release(void *p, size_t n) { (void)n; free(p); }
static void shutdown_plugin(void) {}
int cliproxy_plugin_init(const host_api *host, plugin_api *plugin) {
  if (!host || !plugin || host->abi_version != 1) return 1;
  plugin->abi_version = 1; plugin->call = call; plugin->free_buffer = release; plugin->shutdown = shutdown_plugin; return 0;
}
