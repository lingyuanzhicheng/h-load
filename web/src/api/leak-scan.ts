import http from "@/utils/http";
import type { GroupLeakScanEvent, GroupLeakScanRun, LeakScanConfig } from "@/types/models";

export const leakScanApi = {
  async getConfig(groupId: number): Promise<LeakScanConfig> {
    const res = await http.get(`/groups/${groupId}/leak-scan/config`);
    return res.data;
  },

  async saveConfig(groupId: number, config: LeakScanConfig): Promise<LeakScanConfig> {
    const res = await http.put(`/groups/${groupId}/leak-scan/config`, config);
    return res.data;
  },

  async getStatus(groupId: number): Promise<{ config: LeakScanConfig; run?: GroupLeakScanRun }> {
    const res = await http.get(`/groups/${groupId}/leak-scan/status`, { hideMessage: true });
    return res.data;
  },

  async start(groupId: number): Promise<GroupLeakScanRun> {
    const res = await http.post(`/groups/${groupId}/leak-scan/start`);
    return res.data;
  },

  async stop(groupId: number): Promise<void> {
    await http.post(`/groups/${groupId}/leak-scan/stop`);
  },

  async resume(groupId: number): Promise<GroupLeakScanRun> {
    const res = await http.post(`/groups/${groupId}/leak-scan/resume`);
    return res.data;
  },

  async reset(groupId: number): Promise<GroupLeakScanRun> {
    const res = await http.post(`/groups/${groupId}/leak-scan/reset`);
    return res.data;
  },

  async initialize(groupId: number): Promise<void> {
    await http.post(`/groups/${groupId}/leak-scan/initialize`);
  },

  async events(groupId: number, params?: { run_id?: number; page?: number; page_size?: number }): Promise<{
    run?: GroupLeakScanRun;
    events: GroupLeakScanEvent[];
    pagination: { page: number; page_size: number; total_items: number; total_pages: number };
  }> {
    const res = await http.get(`/groups/${groupId}/leak-scan/events`, { params, hideMessage: true });
    return res.data;
  },
};
