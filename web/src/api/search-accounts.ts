import http from "@/utils/http";
import type { GitHubSearchAccount, SearchAccountStatus, SearchAccountType } from "@/types/models";

export const searchAccountsApi = {
  async list(params?: { type?: SearchAccountType; status?: SearchAccountStatus }): Promise<GitHubSearchAccount[]> {
    const res = await http.get("/search-accounts", { params });
    return res.data || [];
  },

  async create(account: Partial<GitHubSearchAccount>): Promise<GitHubSearchAccount> {
    const res = await http.post("/search-accounts", account);
    return res.data;
  },

  async update(id: number, account: Partial<GitHubSearchAccount>): Promise<GitHubSearchAccount> {
    const res = await http.put(`/search-accounts/${id}`, account);
    return res.data;
  },

  async delete(id: number): Promise<void> {
    await http.delete(`/search-accounts/${id}`);
  },

  async validate(id: number): Promise<GitHubSearchAccount> {
    const res = await http.post(`/search-accounts/${id}/validate`);
    return res.data;
  },

  async validateMany(params?: { type?: SearchAccountType; status?: SearchAccountStatus }): Promise<{ valid: number; invalid: number }> {
    const res = await http.post("/search-accounts/validate", undefined, { params });
    return res.data;
  },
};
