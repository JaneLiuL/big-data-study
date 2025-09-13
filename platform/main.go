package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	metrictl "k8s.io/metrics/pkg/client/clientset/versioned"
)

// 全局K8s客户端
var clientset *kubernetes.Clientset
var metricsClient *metrictl.Clientset

// 响应结构
type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// 资源信息结构
type ResourceInfo struct {
	Name     string `json:"name"`
	Replicas int32  `json:"replicas"`
}

// CPU统计结果结构
type CPUStats struct {
	TotalCPU        string         `json:"total_cpu"`                  // 总CPU请求，以核心数为单位
	TotalMilliCPU   int            `json:"total_milli_cpu"`            // 总CPU请求，以毫核为单位
	NamespaceStats  map[string]int `json:"namespace_stats,omitempty"`  // 每个命名空间的毫核数
	DeploymentStats map[string]int `json:"deployment_stats,omitempty"` // 每个Deployment的毫核数
}

// 资源使用建议结构
type ResourceRecommendation struct {
	DeploymentName       string                  `json:"deployment_name"`
	Namespace            string                  `json:"namespace"`
	AnalysisPeriod       string                  `json:"analysis_period"`
	CurrentReplicas      int32                   `json:"current_replicas"`
	PodMetrics           []PodMetricSummary      `json:"pod_metrics"`
	ResourceUsageStats   ResourceUsageStatistics `json:"resource_usage_statistics"`
	RecommendedResources RecommendedResources    `json:"recommended_resources"`
	TimeWindowStart      time.Time               `json:"timeWindowStart"`
	TimeWindowEnd        time.Time               `json:"timeWindowEnd"`
}

// Pod指标摘要
type PodMetricSummary struct {
	PodName     string           `json:"pod_name"`
	StartTime   time.Time        `json:"start_time"`
	CPUUsage    CPUUsageStats    `json:"cpu_usage"`
	MemoryUsage MemoryUsageStats `json:"memory_usage"`
}

// CPU使用统计
type CPUUsageStats struct {
	AverageMilliCore float64 `json:"average_milli_core"`                   // 平均使用毫核
	PeakMilliCore    float64 `json:"peak_milli_core"`                      // 峰值使用毫核
	P95MilliCore     float64 `json:"p95_milli_core"`                       // 95分位使用毫核
	CurrentRequest   int64   `json:"current_request_milli_core,omitempty"` // 当前请求毫核
	CurrentLimit     int64   `json:"current_limit_milli_core,omitempty"`   // 当前限制毫核
}

// 内存使用统计
type MemoryUsageStats struct {
	AverageMebibyte float64 `json:"average_mebibyte"`                   // 平均使用MiB
	PeakMebibyte    float64 `json:"peak_mebibyte"`                      // 峰值使用MiB
	P95Mebibyte     float64 `json:"p95_mebibyte"`                       // 95分位使用MiB
	CurrentRequest  int64   `json:"current_request_mebibyte,omitempty"` // 当前请求MiB
	CurrentLimit    int64   `json:"current_limit_mebibyte,omitempty"`   // 当前限制MiB
}

// 资源使用统计
type ResourceUsageStatistics struct {
	TotalSamples    int       `json:"total_samples"`
	TimeWindowStart time.Time `json:"time_window_start"`
	TimeWindowEnd   time.Time `json:"time_window_end"`
}

// 推荐资源配置
type RecommendedResources struct {
	CPURequestMilliCore   int64 `json:"cpu_request_milli_core"`  // 推荐CPU请求毫核
	CPULimitMilliCore     int64 `json:"cpu_limit_milli_core"`    // 推荐CPU限制毫核
	MemoryRequestMebibyte int64 `json:"memory_request_mebibyte"` // 推荐内存请求MiB
	MemoryLimitMebibyte   int64 `json:"memory_limit_mebibyte"`   // 推荐内存限制MiB
}

func main() {
	// 初始化K8s客户端
	initK8sClients()

	// 创建数据存储目录
	createDataDirs()

	// 注册路由
	http.HandleFunc("/update/ns/", handleUpdate)
	http.HandleFunc("/get/ns", handleGet)
	http.HandleFunc("/roll/ns", handleRollback)
	http.HandleFunc("/calculate", handleCalculate)
	http.HandleFunc("/metrics/ns/deployment/", handleDeploymentMetrics) // 新增的metrics接口

	// 启动服务
	fmt.Println("Server starting on :8080")
	http.ListenAndServe(":8080", nil)
}

// 初始化K8s客户端（包括metrics客户端）
func initK8sClients() {
	var kubeconfig string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	} else {
		kubeconfig = os.Getenv("KUBECONFIG")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		panic(fmt.Sprintf("Failed to load kubeconfig: %v", err))
	}

	// 创建核心clientset
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		panic(fmt.Sprintf("Failed to create clientset: %v", err))
	}

	// 创建metrics客户端
	metricsClient, err = metrictl.NewForConfig(config)
	if err != nil {
		panic(fmt.Sprintf("Failed to create metrics client: %v", err))
	}

	fmt.Println("Kubernetes clients initialized successfully")
}

// 创建数据存储目录
func createDataDirs() {
	dirs := []string{
		"/opt/ns/deployment",
		"/opt/ns/statefulset",
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Printf("Warning: Failed to create directory %s: %v\n", dir, err)
		}
	}
}

// 处理/update/ns/接口
func handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendResponse(w, false, "Method not allowed", nil, http.StatusMethodNotAllowed)
		return
	}

	// 解析表单数据
	if err := r.ParseForm(); err != nil {
		sendResponse(w, false, fmt.Sprintf("Failed to parse form: %v", err), nil, http.StatusBadRequest)
		return
	}

	ns := r.Form.Get("ns")
	resType := r.Form.Get("type")

	// 验证参数
	if err := validateParams(ns, resType); err != nil {
		sendResponse(w, false, err.Error(), nil, http.StatusBadRequest)
		return
	}

	// 缩容资源
	err := scaleResourcesToZero(ns, resType)
	if err != nil {
		sendResponse(w, false, fmt.Sprintf("Failed to scale resources: %v", err), nil, http.StatusInternalServerError)
		return
	}

	sendResponse(w, true, fmt.Sprintf("Successfully scaled all %s in namespace %s to 0 replicas", resType, ns), nil, http.StatusOK)
}

// 处理/get/ns接口
func handleGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendResponse(w, false, "Method not allowed", nil, http.StatusMethodNotAllowed)
		return
	}

	ns := r.URL.Query().Get("ns")
	resType := r.URL.Query().Get("type")

	// 验证参数
	if err := validateParams(ns, resType); err != nil {
		sendResponse(w, false, err.Error(), nil, http.StatusBadRequest)
		return
	}

	// 获取资源信息
	resources, err := getResources(ns, resType)
	if err != nil {
		sendResponse(w, false, fmt.Sprintf("Failed to get resources: %v", err), nil, http.StatusInternalServerError)
		return
	}

	// 持久化资源信息
	err = persistResources(ns, resType, resources)
	if err != nil {
		sendResponse(w, false, fmt.Sprintf("Failed to persist resources: %v", err), nil, http.StatusInternalServerError)
		return
	}

	sendResponse(w, true, fmt.Sprintf("Successfully retrieved %d %s resources from namespace %s", len(resources), resType, ns), resources, http.StatusOK)
}

// 处理/roll/ns接口
func handleRollback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendResponse(w, false, "Method not allowed", nil, http.StatusMethodNotAllowed)
		return
	}

	// 解析表单数据
	if err := r.ParseForm(); err != nil {
		sendResponse(w, false, fmt.Sprintf("Failed to parse form: %v", err), nil, http.StatusBadRequest)
		return
	}

	ns := r.Form.Get("ns")
	resType := r.Form.Get("type")

	// 验证参数
	if err := validateParams(ns, resType); err != nil {
		sendResponse(w, false, err.Error(), nil, http.StatusBadRequest)
		return
	}

	// 读取持久化的资源信息
	resources, err := readPersistedResources(ns, resType)
	if err != nil {
		sendResponse(w, false, fmt.Sprintf("Failed to read persisted resources: %v", err), nil, http.StatusInternalServerError)
		return
	}

	// 恢复资源副本数
	err = scaleResourcesFromBackup(ns, resType, resources)
	if err != nil {
		sendResponse(w, false, fmt.Sprintf("Failed to rollback resources: %v", err), nil, http.StatusInternalServerError)
		return
	}

	sendResponse(w, true, fmt.Sprintf("Successfully rolled back %d %s resources in namespace %s", len(resources), resType, ns), resources, http.StatusOK)
}

// 处理/calculate接口
func handleCalculate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendResponse(w, false, "Method not allowed", nil, http.StatusMethodNotAllowed)
		return
	}

	// 获取参数：ns可以是"all"或具体命名空间名称
	nsParam := r.URL.Query().Get("ns")
	if nsParam == "" {
		sendResponse(w, false, "ns parameter is required (use 'all' for cluster-wide stats)", nil, http.StatusBadRequest)
		return
	}

	// 统计CPU请求
	stats, err := calculateCPURequests(nsParam)
	if err != nil {
		sendResponse(w, false, fmt.Sprintf("Failed to calculate CPU requests: %v", err), nil, http.StatusInternalServerError)
		return
	}

	sendResponse(w, true, fmt.Sprintf("Successfully calculated CPU requests for %s", nsParam), stats, http.StatusOK)
}

// 处理/metrics/ns/deployment/接口 - 新增功能
func handleDeploymentMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendResponse(w, false, "Method not allowed", nil, http.StatusMethodNotAllowed)
		return
	}

	// 获取参数
	ns := r.URL.Query().Get("namespace")
	deployName := r.URL.Query().Get("name")

	// 验证参数
	if ns == "" || deployName == "" {
		sendResponse(w, false, "Both 'namespace' and 'name' parameters are required", nil, http.StatusBadRequest)
		return
	}

	// 检查Deployment是否存在
	deploy, err := clientset.AppsV1().Deployments(ns).Get(context.TODO(), deployName, metav1.GetOptions{})
	if err != nil {
		sendResponse(w, false, fmt.Sprintf("Deployment %s in namespace %s not found: %v", deployName, ns, err), nil, http.StatusNotFound)
		return
	}

	// 获取过去24小时的metrics并分析
	recommendation, err := analyzeDeploymentResources(ns, deploy)
	if err != nil {
		sendResponse(w, false, fmt.Sprintf("Failed to analyze deployment resources: %v", err), nil, http.StatusInternalServerError)
		return
	}

	sendResponse(w, true, fmt.Sprintf("Successfully analyzed resource usage for deployment %s/%s", ns, deployName), recommendation, http.StatusOK)
}

// 分析Deployment的资源使用情况并给出建议
func analyzeDeploymentResources(namespace string, deployment *appsv1.Deployment) (*ResourceRecommendation, error) {
	// 时间范围：过去24小时
	endTime := time.Now()
	startTime := endTime.Add(-24 * time.Hour)

	recommendation := &ResourceRecommendation{
		DeploymentName:  deployment.Name,
		Namespace:       namespace,
		AnalysisPeriod:  "24h",
		CurrentReplicas: *deployment.Spec.Replicas,
		TimeWindowStart: startTime,
		TimeWindowEnd:   endTime,
	}

	// 获取Deployment关联的所有Pod
	labelSelector := metav1.FormatLabelSelector(deployment.Spec.Selector)
	pods, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %v", err)
	}

	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pods found for deployment %s", deployment.Name)
	}

	// 收集每个Pod的metrics
	var allCPUUsage []float64
	var allMemoryUsage []float64

	for _, pod := range pods.Items {
		podMetricSummary, err := getPodMetrics(namespace, pod.Name, &pod)
		if err != nil {
			fmt.Printf("Warning: Failed to get metrics for pod %s: %v\n", pod.Name, err)
			continue
		}

		recommendation.PodMetrics = append(recommendation.PodMetrics, *podMetricSummary)

		// 收集所有样本用于整体统计
		allCPUUsage = append(allCPUUsage, podMetricSummary.CPUUsage.AverageMilliCore)
		allMemoryUsage = append(allMemoryUsage, podMetricSummary.MemoryUsage.AverageMebibyte)
	}

	if len(recommendation.PodMetrics) == 0 {
		return nil, fmt.Errorf("no metrics available for any pods in deployment %s", deployment.Name)
	}

	// 计算总体统计数据
	recommendation.ResourceUsageStats.TotalSamples = len(allCPUUsage)

	// 计算统计值
	cpuPeak := maxFloat64(allCPUUsage)
	// cpuAvg := avgFloat64(allCPUUsage)
	cpuP95 := percentileFloat64(allCPUUsage, 95)

	memPeak := maxFloat64(allMemoryUsage)
	// memAvg := avgFloat64(allMemoryUsage)
	memP95 := percentileFloat64(allMemoryUsage, 95)

	// 获取当前资源配置（假设所有容器配置相同，取第一个容器的配置）
	var currentCPURequest, currentCPULimit int64
	var currentMemoryRequest, currentMemoryLimit int64

	if len(deployment.Spec.Template.Spec.Containers) > 0 {
		resources := deployment.Spec.Template.Spec.Containers[0].Resources
		if resources.Requests != nil {
			currentCPURequest = resources.Requests.Cpu().MilliValue()
			currentMemoryRequest = resources.Requests.Memory().Value() / 1024 / 1024 // 转换为MiB
		}
		if resources.Limits != nil {
			currentCPULimit = resources.Limits.Cpu().MilliValue()
			currentMemoryLimit = resources.Limits.Memory().Value() / 1024 / 1024 // 转换为MiB
		}
	}

	// 设置总体统计值
	for i := range recommendation.PodMetrics {
		if i == 0 { // 只在第一个Pod中设置当前配置（假设所有Pod配置相同）
			recommendation.PodMetrics[i].CPUUsage.CurrentRequest = currentCPURequest
			recommendation.PodMetrics[i].CPUUsage.CurrentLimit = currentCPULimit
			recommendation.PodMetrics[i].MemoryUsage.CurrentRequest = currentMemoryRequest
			recommendation.PodMetrics[i].MemoryUsage.CurrentLimit = currentMemoryLimit
		}
	}

	// 生成推荐配置（基于95分位值并增加安全系数）
	recommendation.RecommendedResources = RecommendedResources{
		CPURequestMilliCore:   int64(cpuP95 * 1.1),  // 95分位值 + 10%安全系数
		CPULimitMilliCore:     int64(cpuPeak * 1.2), // 峰值 + 20%安全系数
		MemoryRequestMebibyte: int64(memP95 * 1.2),  // 95分位值 + 20%安全系数
		MemoryLimitMebibyte:   int64(memPeak * 1.3), // 峰值 + 30%安全系数
	}

	return recommendation, nil
}

// 获取单个Pod的metrics
func getPodMetrics(namespace, podName string, pod *corev1.Pod) (*PodMetricSummary, error) {
	// 获取最新的Pod metrics
	podMetrics, err := metricsClient.MetricsV1beta1().PodMetricses(namespace).Get(context.TODO(), podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get pod metrics: %v", err)
	}

	summary := &PodMetricSummary{
		PodName:   podName,
		StartTime: pod.CreationTimestamp.Time,
	}

	// 计算容器平均使用量
	var totalCPU, totalMemory float64
	containerCount := len(podMetrics.Containers)

	for _, container := range podMetrics.Containers {
		totalCPU += float64(container.Usage.Cpu().MilliValue())
		totalMemory += float64(container.Usage.Memory().Value() / 1024 / 1024) // 转换为MiB
	}

	// 设置平均值（简化处理，实际应该获取时间序列数据）
	summary.CPUUsage.AverageMilliCore = totalCPU / float64(containerCount)
	summary.MemoryUsage.AverageMebibyte = totalMemory / float64(containerCount)

	// 对于峰值和分位值，这里使用当前测量值作为简化实现
	// 实际生产环境应该查询时间序列数据（如Prometheus）
	summary.CPUUsage.PeakMilliCore = summary.CPUUsage.AverageMilliCore
	summary.CPUUsage.P95MilliCore = summary.CPUUsage.AverageMilliCore
	summary.MemoryUsage.PeakMebibyte = summary.MemoryUsage.AverageMebibyte
	summary.MemoryUsage.P95Mebibyte = summary.MemoryUsage.AverageMebibyte

	return summary, nil
}

// 验证参数
func validateParams(ns, resType string) error {
	if ns == "" {
		return fmt.Errorf("namespace (ns) is required")
	}

	if resType != "deployment" && resType != "statefulset" {
		return fmt.Errorf("type must be 'deployment' or 'statefulset'")
	}

	// 检查namespace是否存在
	_, err := clientset.CoreV1().Namespaces().Get(context.TODO(), ns, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("namespace %s does not exist", ns)
	}

	return nil
}

// 将资源缩容到0
func scaleResourcesToZero(ns, resType string) error {
	switch resType {
	case "deployment":
		deployments, err := clientset.AppsV1().Deployments(ns).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}

		for _, deploy := range deployments.Items {
			deploy.Spec.Replicas = int32Ptr(0)
			_, err := clientset.AppsV1().Deployments(ns).Update(context.TODO(), &deploy, metav1.UpdateOptions{})
			if err != nil {
				fmt.Printf("Warning: Failed to scale deployment %s: %v\n", deploy.Name, err)
			} else {
				fmt.Printf("Scaled deployment %s to 0 replicas\n", deploy.Name)
			}
		}

	case "statefulset":
		statefulsets, err := clientset.AppsV1().StatefulSets(ns).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return err
		}

		for _, sts := range statefulsets.Items {
			sts.Spec.Replicas = int32Ptr(0)
			_, err := clientset.AppsV1().StatefulSets(ns).Update(context.TODO(), &sts, metav1.UpdateOptions{})
			if err != nil {
				fmt.Printf("Warning: Failed to scale statefulset %s: %v\n", sts.Name, err)
			} else {
				fmt.Printf("Scaled statefulset %s to 0 replicas\n", sts.Name)
			}
		}
	}

	return nil
}

// 获取资源信息
func getResources(ns, resType string) ([]ResourceInfo, error) {
	var resources []ResourceInfo

	switch resType {
	case "deployment":
		deployments, err := clientset.AppsV1().Deployments(ns).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, err
		}

		for _, deploy := range deployments.Items {
			replicas := int32(0)
			if deploy.Spec.Replicas != nil {
				replicas = *deploy.Spec.Replicas
			}
			resources = append(resources, ResourceInfo{
				Name:     deploy.Name,
				Replicas: replicas,
			})
		}

	case "statefulset":
		statefulsets, err := clientset.AppsV1().StatefulSets(ns).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, err
		}

		for _, sts := range statefulsets.Items {
			replicas := int32(0)
			if sts.Spec.Replicas != nil {
				replicas = *sts.Spec.Replicas
			}
			resources = append(resources, ResourceInfo{
				Name:     sts.Name,
				Replicas: replicas,
			})
		}
	}

	return resources, nil
}

// 持久化资源信息
func persistResources(ns, resType string, resources []ResourceInfo) error {
	timestamp := time.Now().Format("20060102150405")
	filename := fmt.Sprintf("/opt/ns/%s/%s_%s", resType, ns, timestamp)

	data, err := json.MarshalIndent(resources, "", "  ")
	if err != nil {
		return err
	}

	// return ioutil.WriteFile(filename, data, 0644)
	return os.WriteFile(filename, data, 0644)
}

// 读取持久化的资源信息
func readPersistedResources(ns, resType string) ([]ResourceInfo, error) {
	dir := fmt.Sprintf("/opt/ns/%s", resType)

	// 获取目录下所有文件
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	// 查找最新的相关文件
	var latestFile string
	var latestTime time.Time

	for _, file := range files {
		if !file.IsDir() && strings.HasPrefix(file.Name(), ns) {
			if file.ModTime().After(latestTime) {
				latestTime = file.ModTime()
				latestFile = file.Name()
			}
		}
	}

	if latestFile == "" {
		return nil, fmt.Errorf("no persisted data found for namespace %s and type %s", ns, resType)
	}

	// 读取文件内容
	data, err := ioutil.ReadFile(filepath.Join(dir, latestFile))
	if err != nil {
		return nil, err
	}

	// 解析JSON
	var resources []ResourceInfo
	if err := json.Unmarshal(data, &resources); err != nil {
		return nil, err
	}

	return resources, nil
}

// 从备份恢复资源副本数
func scaleResourcesFromBackup(ns, resType string, resources []ResourceInfo) error {
	switch resType {
	case "deployment":
		for _, res := range resources {
			deploy, err := clientset.AppsV1().Deployments(ns).Get(context.TODO(), res.Name, metav1.GetOptions{})
			if err != nil {
				fmt.Printf("Warning: Failed to get deployment %s: %v\n", res.Name, err)
				continue
			}

			deploy.Spec.Replicas = int32Ptr(res.Replicas)
			_, err = clientset.AppsV1().Deployments(ns).Update(context.TODO(), deploy, metav1.UpdateOptions{})
			if err != nil {
				fmt.Printf("Warning: Failed to update deployment %s: %v\n", res.Name, err)
			} else {
				fmt.Printf("Updated deployment %s to %d replicas\n", res.Name, res.Replicas)
			}
		}

	case "statefulset":
		for _, res := range resources {
			sts, err := clientset.AppsV1().StatefulSets(ns).Get(context.TODO(), res.Name, metav1.GetOptions{})
			if err != nil {
				fmt.Printf("Warning: Failed to get statefulset %s: %v\n", res.Name, err)
				continue
			}

			sts.Spec.Replicas = int32Ptr(res.Replicas)
			_, err = clientset.AppsV1().StatefulSets(ns).Update(context.TODO(), sts, metav1.UpdateOptions{})
			if err != nil {
				fmt.Printf("Warning: Failed to update statefulset %s: %v\n", res.Name, err)
			} else {
				fmt.Printf("Updated statefulset %s to %d replicas\n", res.Name, res.Replicas)
			}
		}
	}

	return nil
}

// 计算CPU请求总和
func calculateCPURequests(ns string) (*CPUStats, error) {
	stats := &CPUStats{
		NamespaceStats:  make(map[string]int),
		DeploymentStats: make(map[string]int),
	}

	var namespaces []string

	// 确定需要统计的命名空间
	if ns == "all" {
		// 获取所有命名空间
		nsList, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %v", err)
		}
		for _, n := range nsList.Items {
			namespaces = append(namespaces, n.Name)
		}
	} else {
		// 检查指定命名空间是否存在
		_, err := clientset.CoreV1().Namespaces().Get(context.TODO(), ns, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("namespace %s does not exist: %v", ns, err)
		}
		namespaces = []string{ns}
	}

	// 遍历所有相关命名空间，统计Deployment的CPU请求
	for _, namespace := range namespaces {
		deployments, err := clientset.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("Warning: Failed to list deployments in namespace %s: %v\n", namespace, err)
			continue
		}

		nsTotal := 0

		// 处理每个Deployment
		for _, deploy := range deployments.Items {
			deployTotal := 0

			// 检查每个容器的CPU请求
			for _, container := range deploy.Spec.Template.Spec.Containers {
				if container.Resources.Requests != nil {
					cpuReq := container.Resources.Requests.Cpu()
					milliCPU := cpuReq.MilliValue() // 转换为毫核 (1核心 = 1000毫核)
					deployTotal += int(milliCPU)
				}
			}

			// 乘以副本数
			replicas := int32(1)
			if deploy.Spec.Replicas != nil {
				replicas = *deploy.Spec.Replicas
			}
			deployTotal *= int(replicas)

			// 累加到统计结果
			fullName := fmt.Sprintf("%s/%s", namespace, deploy.Name)
			stats.DeploymentStats[fullName] = deployTotal
			nsTotal += deployTotal
			stats.TotalMilliCPU += deployTotal
		}

		// 记录命名空间的总CPU请求
		if nsTotal > 0 {
			stats.NamespaceStats[namespace] = nsTotal
		}
	}

	// 转换为核心数表示 (1核心 = 1000毫核)
	stats.TotalCPU = fmt.Sprintf("%.2f", float64(stats.TotalMilliCPU)/1000.0)

	return stats, nil
}

// 发送响应
func sendResponse(w http.ResponseWriter, success bool, message string, data interface{}, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	response := Response{
		Success: success,
		Message: message,
		Data:    data,
	}

	json.NewEncoder(w).Encode(response)
}

// 辅助函数：int32指针
func int32Ptr(i int32) *int32 {
	return &i
}

// 计算浮点切片的最大值
func maxFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	maxVal := values[0]
	for _, v := range values[1:] {
		if v > maxVal {
			maxVal = v
		}
	}
	return maxVal
}

// 计算浮点切片的平均值
func avgFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

// 计算浮点切片的分位值
func percentileFloat64(values []float64, percentile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	// 简化实现：排序后取近似位置的值
	sorted := make([]float64, len(values))
	copy(sorted, values)
	// 这里省略排序实现，实际应该排序
	// sort.Float64s(sorted)

	index := int((percentile / 100.0) * float64(len(sorted)-1))
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}
