package secretgenerator

//const (
//	ServiceName = "secretgenerator"
//)
//
////nolint:gochecknoinits
//func init() {
//	sr.Add(&serviceHandler{})
//}
//
//type serviceHandler struct {
//}
//
//func (h *serviceHandler) Init(_ common.Platform) error {
//	return nil
//}
//
//func (h *serviceHandler) GetName() string {
//	return ServiceName
//}
//
//func (h *serviceHandler) GetManagementState(_ common.Platform) operatorv1.ManagementState {
//	return operatorv1.Managed
//}
//
//func (h *serviceHandler) NewReconciler(ctx context.Context, mgr ctrl.Manager) error {
//	rec := &SecretGeneratorReconciler{
//		Client: mgr.GetClient(),
//	}
//
//	if err := rec.SetupWithManager(ctx, mgr); err != nil {
//		return fmt.Errorf("could not create the %s controller: %w", ServiceName, err)
//	}
//
//	return nil
//}
