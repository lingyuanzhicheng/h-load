declare module "axios" {
  export interface AxiosRequestConfig {
    url?: string;
    method?: string;
    baseURL?: string;
    timeout?: number;
    headers?: Record<string, any> & { common?: Record<string, string> };
    params?: any;
    hideMessage?: boolean;
  }

  export interface AxiosResponse<T = any> {
    data: T;
    config: AxiosRequestConfig & { method?: string };
  }

  export interface AxiosError {
    response?: { status: number; data?: { message?: string } };
    request?: unknown;
  }

  export interface AxiosInstance {
    defaults: AxiosRequestConfig;
    interceptors: {
      request: { use(fn: (config: AxiosRequestConfig) => AxiosRequestConfig): void };
      response: { use(fn: (response: AxiosResponse) => any, err?: (error: AxiosError) => any): void };
    };
    get<T = any>(url: string, config?: AxiosRequestConfig): Promise<any>;
    post<T = any>(url: string, data?: unknown, config?: AxiosRequestConfig): Promise<any>;
    put<T = any>(url: string, data?: unknown, config?: AxiosRequestConfig): Promise<any>;
    delete<T = any>(url: string, config?: AxiosRequestConfig): Promise<any>;
  }

  const axios: {
    defaults: AxiosRequestConfig;
    create(config?: AxiosRequestConfig): AxiosInstance;
    get<T = any>(url: string, config?: AxiosRequestConfig): Promise<any>;
  };

  export default axios;
}
